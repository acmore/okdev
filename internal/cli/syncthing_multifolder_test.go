package cli

import (
	"strings"
	"testing"

	syncengine "github.com/acmore/okdev/internal/sync"
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
