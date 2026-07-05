package cli

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func TestResolveSyncFoldersPerPathAndOverride(t *testing.T) {
	pairs := []syncengine.Pair{
		{Local: ".", Remote: "/workspace", Direction: "bi"},
		{Local: "./results", Remote: "/data/results", Direction: "down"},
		{Local: "./datasets", Remote: "/data/sets", Direction: "up"},
	}

	// Empty mode: each folder follows its own direction; no bootstrap.
	folders, err := resolveSyncFolders("sess", "", pairs)
	if err != nil {
		t.Fatalf("resolveSyncFolders: %v", err)
	}
	if folders[0].mode != "bi" || folders[1].mode != "down" || folders[2].mode != "up" {
		t.Fatalf("unexpected per-path modes: %+v", folders)
	}
	for _, f := range folders {
		if f.twoPhase {
			t.Fatalf("no folder should bootstrap in empty mode: %+v", f)
		}
	}

	// two-phase bootstraps only bi folders; directional ones configure
	// their final types directly.
	folders, err = resolveSyncFolders("sess", "two-phase", pairs)
	if err != nil {
		t.Fatalf("resolveSyncFolders: %v", err)
	}
	if !folders[0].twoPhase || folders[1].twoPhase || folders[2].twoPhase {
		t.Fatalf("two-phase must apply to bi folders only: %+v", folders)
	}
	if folders[1].mode != "down" || folders[2].mode != "up" {
		t.Fatalf("directional folders must keep their modes under two-phase: %+v", folders)
	}

	// An explicit mode forces every folder.
	folders, err = resolveSyncFolders("sess", "down", pairs)
	if err != nil {
		t.Fatalf("resolveSyncFolders: %v", err)
	}
	for _, f := range folders {
		if f.mode != "down" || f.twoPhase {
			t.Fatalf("explicit mode must force all folders: %+v", f)
		}
	}

	// Defensive: an empty direction (pair built without ParsePairs) is bi.
	folders, err = resolveSyncFolders("sess", "two-phase", []syncengine.Pair{{Local: ".", Remote: "/w"}})
	if err != nil {
		t.Fatalf("resolveSyncFolders: %v", err)
	}
	if folders[0].mode != "bi" || !folders[0].twoPhase {
		t.Fatalf("empty direction must default to bi: %+v", folders[0])
	}
}

func TestSessionSyncFolderIDStability(t *testing.T) {
	primary := syncengine.Pair{Local: ".", Remote: "/workspace"}
	extraA := syncengine.Pair{Local: "./results", Remote: "/data/results"}
	extraB := syncengine.Pair{Local: "./datasets", Remote: "/data/sets"}

	if got := sessionSyncFolderID("sess", 0, primary); got != "okdev-sess" {
		t.Fatalf("primary mapping must keep the legacy folder ID, got %q", got)
	}
	idA := sessionSyncFolderID("sess", 1, extraA)
	idB := sessionSyncFolderID("sess", 2, extraB)
	if !strings.HasPrefix(idA, "okdev-sess-") || len(idA) != len("okdev-sess-")+8 {
		t.Fatalf("unexpected secondary folder ID shape: %q", idA)
	}
	if idA == idB {
		t.Fatal("different pairs must get different folder IDs")
	}
	// Stable under reordering: the ID depends on the pair, not the index.
	if got := sessionSyncFolderID("sess", 2, extraA); got != idA {
		t.Fatalf("secondary folder ID must be index-independent: %q vs %q", got, idA)
	}
}

func TestPruneManagedFoldersFromConfig(t *testing.T) {
	cfg := map[string]any{
		"folders": []any{
			map[string]any{"id": "okdev-sess"},                  // primary, kept
			map[string]any{"id": "okdev-sess-aabbccdd"},         // managed extra, stale -> pruned
			map[string]any{"id": "okdev-sess-2"},                // ANOTHER session's primary -> untouched
			map[string]any{"id": "okdev-sess-2-aabbccdd"},       // another session's extra -> untouched
			map[string]any{"id": "user-folder"},                 // user-defined -> untouched
			map[string]any{"id": "okdev-sess-not8hexnot8hex00"}, // wrong suffix shape -> untouched
		},
	}
	changed, err := pruneManagedFoldersFromConfig(cfg, "sess", map[string]bool{"okdev-sess": true})
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if !changed {
		t.Fatal("expected stale managed folder to be pruned")
	}
	var ids []string
	for _, f := range cfg["folders"].([]any) {
		ids = append(ids, f.(map[string]any)["id"].(string))
	}
	want := []string{"okdev-sess", "okdev-sess-2", "okdev-sess-2-aabbccdd", "user-folder", "okdev-sess-not8hexnot8hex00"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("unexpected surviving folders: %v (want %v)", ids, want)
	}

	// No-op when everything is kept.
	changed, err = pruneManagedFoldersFromConfig(cfg, "sess", map[string]bool{"okdev-sess": true})
	if err != nil || changed {
		t.Fatalf("expected no-op prune, changed=%v err=%v", changed, err)
	}
}

func TestRunSyncthingSyncMarksBootstrapCompleteWithoutBiFolders(t *testing.T) {
	// Regression: `okdev up` waits for the bootstrap-complete handoff
	// whenever it starts sync in two-phase mode. With only directional
	// mappings no folder runs the two-phase protocol, but the marker must
	// still be written or the caller hangs until its handoff timeout.
	home := t.TempDir()
	t.Setenv("HOME", home)

	origEnsureBinary := syncthingEnsureBinaryFn
	origResolveTargetRef := resolveTargetRefFn
	origExec := execInSyncthingContainerFn
	origPF := startSyncthingPortForwardFn
	origReadKey := readRemoteSyncthingAPIKeyFn
	origStartLocal := startAndWaitForLocalSyncthingFn
	origWaitAPI := waitSyncthingAPIFn
	origDeviceID := syncthingDeviceIDFn
	origMarkReady := markSyncthingReadyFn
	origMarkComplete := markSyncthingBootstrapCompleteFn
	origTwoPhase := runTwoPhaseInitialSyncFn
	origConfigure := configureSyncthingPeerFn
	origPrune := pruneManagedSessionFoldersFn
	origVerify := verifyRemoteRootSharedFn
	origSaveHash := saveSyncthingConfigHashFn
	origStopOwned := stopOwnedLocalSyncthingFn
	origHealth := runSyncHealthLoopFn
	t.Cleanup(func() {
		syncthingEnsureBinaryFn = origEnsureBinary
		resolveTargetRefFn = origResolveTargetRef
		execInSyncthingContainerFn = origExec
		startSyncthingPortForwardFn = origPF
		readRemoteSyncthingAPIKeyFn = origReadKey
		startAndWaitForLocalSyncthingFn = origStartLocal
		waitSyncthingAPIFn = origWaitAPI
		syncthingDeviceIDFn = origDeviceID
		markSyncthingReadyFn = origMarkReady
		markSyncthingBootstrapCompleteFn = origMarkComplete
		runTwoPhaseInitialSyncFn = origTwoPhase
		configureSyncthingPeerFn = origConfigure
		pruneManagedSessionFoldersFn = origPrune
		verifyRemoteRootSharedFn = origVerify
		saveSyncthingConfigHashFn = origSaveHash
		stopOwnedLocalSyncthingFn = origStopOwned
		runSyncHealthLoopFn = origHealth
	})

	syncthingEnsureBinaryFn = func(context.Context, string, bool) (string, error) { return "/tmp/syncthing", nil }
	resolveTargetRefFn = func(context.Context, *Options, *config.DevEnvironment, string, string, targetResolverClient) (workload.TargetRef, error) {
		return workload.TargetRef{PodName: "pod-0"}, nil
	}
	execInSyncthingContainerFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string, string) ([]byte, error) {
		return nil, nil
	}
	startSyncthingPortForwardFn = func(context.Context, *Options, string, string) (context.CancelFunc, string, string, error) {
		return func() {}, "http://remote", "tcp://127.0.0.1:22000", nil
	}
	readRemoteSyncthingAPIKeyFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string) (string, error) {
		return "remote-key", nil
	}
	startAndWaitForLocalSyncthingFn = func(context.Context, string, string) (string, string, int, error) {
		return "http://local", "local-key", 4321, nil
	}
	waitSyncthingAPIFn = func(context.Context, string, string, time.Duration) error { return nil }
	syncthingDeviceIDFn = func(_ context.Context, base, _ string) (string, error) {
		if base == "http://local" {
			return "LOCAL", nil
		}
		return "REMOTE", nil
	}
	markSyncthingReadyFn = func(string) error { return nil }
	twoPhaseCalls := 0
	runTwoPhaseInitialSyncFn = func(context.Context, io.Writer, string, string, string, string, string, string, string, string, string, string, syncthingSyncConfig) error {
		twoPhaseCalls++
		return nil
	}
	configureSyncthingPeerFn = func(context.Context, string, string, string, string, string, string, string, string, int, int, bool, bool, bool, int) error {
		return nil
	}
	pruneManagedSessionFoldersFn = func(context.Context, string, string, string, map[string]bool) error { return nil }
	verifyRemoteRootSharedFn = func(context.Context, *kube.Client, string, string, string, syncFolder) error { return nil }
	saveSyncthingConfigHashFn = func(string, string) error { return nil }
	stopOwnedLocalSyncthingFn = func(string, int) error { return nil }
	runSyncHealthLoopFn = func(<-chan os.Signal, io.Writer, syncHealthChecker) {}
	completeCalls := 0
	markSyncthingBootstrapCompleteFn = func(string) error {
		completeCalls++
		return nil
	}

	localA := filepath.Join(home, "code")
	localB := filepath.Join(home, "collected")
	cfg := &config.DevEnvironment{}
	cfg.SetDefaults()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := runSyncthingSync(cmd, &Options{}, cfg, "default", "sess-directional", "two-phase", []syncengine.Pair{
		{Local: localA, Remote: "/workspace", Direction: "up"},
		{Local: localB, Remote: "/data/results", Direction: "down"},
	}, nil)
	if err != nil {
		t.Fatalf("runSyncthingSync: %v", err)
	}
	if twoPhaseCalls != 0 {
		t.Fatalf("directional folders must not run the two-phase protocol, got %d calls", twoPhaseCalls)
	}
	if completeCalls != 1 {
		t.Fatalf("bootstrap-complete marker must be written exactly once, got %d", completeCalls)
	}
}
