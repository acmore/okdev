package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type fakePodSummaryReader struct {
	summary *kube.PodSummary
	err     error
}

func (f fakePodSummaryReader) GetPodSummary(context.Context, string, string) (*kube.PodSummary, error) {
	return f.summary, f.err
}

func TestNewSyncCmdDefaultsToBackground(t *testing.T) {
	cmd := newSyncCmd(&Options{})
	flag := cmd.Flags().Lookup("background")
	if flag == nil {
		t.Fatal("expected background flag")
	}
	if flag.DefValue != "true" {
		t.Fatalf("expected background default true, got %q", flag.DefValue)
	}
	if cmd.Flags().Lookup("foreground") == nil {
		t.Fatal("expected foreground flag")
	}
}

func TestResetSyncthingSessionStateRemovesLocalState(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	sessionName := "test-sync-reset"
	sessionHome, err := localSyncthingHome(sessionName)
	if err != nil {
		t.Fatalf("localSyncthingHome: %v", err)
	}
	stateFile := filepath.Join(sessionHome, "config", "state.txt")
	if err := os.MkdirAll(filepath.Dir(stateFile), 0o755); err != nil {
		t.Fatalf("mkdir state dir: %v", err)
	}
	if err := os.WriteFile(stateFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}

	if err := resetSyncthingSessionState(sessionName); err != nil {
		t.Fatalf("resetSyncthingSessionState: %v", err)
	}

	if _, err := os.Stat(stateFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale state file to be removed, got err=%v", err)
	}
	if info, err := os.Stat(sessionHome); err != nil {
		t.Fatalf("expected session home to exist after reset: %v", err)
	} else if !info.IsDir() {
		t.Fatalf("expected session home to remain a directory")
	}
}

func TestRefreshSyncthingSessionProcessesPreservesLocalState(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	sessionName := "test-sync-refresh"
	sessionHome, err := localSyncthingHome(sessionName)
	if err != nil {
		t.Fatalf("localSyncthingHome: %v", err)
	}
	stateFile := filepath.Join(sessionHome, "config.xml")
	if err := os.WriteFile(stateFile, []byte("<config/>"), 0o644); err != nil {
		t.Fatalf("write state file: %v", err)
	}
	pidPath, err := syncthingPIDPath(sessionName)
	if err != nil {
		t.Fatalf("syncthingPIDPath: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("write pid file: %v", err)
	}

	if err := refreshSyncthingSessionProcesses(sessionName); err != nil {
		t.Fatalf("refreshSyncthingSessionProcesses: %v", err)
	}

	if _, err := os.Stat(stateFile); err != nil {
		t.Fatalf("expected state file to remain after refresh: %v", err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale pid file to be removed, got err=%v", err)
	}
}

func TestReadSyncthingPID(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "sync.pid")
	if err := os.WriteFile(pidPath, []byte("12345\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, ok := readSyncthingPID(pidPath)
	if !ok || got != 12345 {
		t.Fatalf("unexpected pid parse result pid=%d ok=%v", got, ok)
	}
}

func TestReadSyncthingPIDRejectsInvalidValue(t *testing.T) {
	dir := t.TempDir()
	pidPath := filepath.Join(dir, "sync.pid")
	if err := os.WriteFile(pidPath, []byte("abc\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := readSyncthingPID(pidPath); ok {
		t.Fatal("expected invalid pid to be rejected")
	}
}

func TestEnsureSyncthingTargetSessionStateInitialSaveDoesNotReset(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	reset, err := ensureSyncthingTargetSessionState(context.Background(), fakePodSummaryReader{
		summary: &kube.PodSummary{Name: "pod-a", CreatedAt: time.Unix(100, 0)},
	}, "default", "sess-a", "pod-a")
	if err != nil {
		t.Fatalf("ensureSyncthingTargetSessionState: %v", err)
	}
	if reset {
		t.Fatal("did not expect initial state to reset")
	}
	state, err := loadSyncthingTargetSessionState("sess-a")
	if err != nil {
		t.Fatalf("loadSyncthingTargetSessionState: %v", err)
	}
	if state.PodName != "pod-a" {
		t.Fatalf("unexpected saved pod name %q", state.PodName)
	}
}

func TestEnsureSyncthingTargetSessionStateResetsWhenPodRecreated(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	sessionName := "sess-b"
	sessionHome, err := localSyncthingHome(sessionName)
	if err != nil {
		t.Fatalf("localSyncthingHome: %v", err)
	}
	staleFile := filepath.Join(sessionHome, "config", "state.txt")
	if err := os.MkdirAll(filepath.Dir(staleFile), 0o755); err != nil {
		t.Fatalf("mkdir stale dir: %v", err)
	}
	if err := os.WriteFile(staleFile, []byte("stale"), 0o644); err != nil {
		t.Fatalf("write stale file: %v", err)
	}
	if err := saveSyncthingTargetSessionState(sessionName, syncthingSessionTargetState{
		PodName:   "pod-a",
		CreatedAt: time.Unix(100, 0).UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("saveSyncthingTargetSessionState: %v", err)
	}

	reset, err := ensureSyncthingTargetSessionState(context.Background(), fakePodSummaryReader{
		summary: &kube.PodSummary{Name: "pod-a", CreatedAt: time.Unix(200, 0)},
	}, "default", sessionName, "pod-a")
	if err != nil {
		t.Fatalf("ensureSyncthingTargetSessionState: %v", err)
	}
	if !reset {
		t.Fatal("expected recreated pod to trigger reset")
	}
	if _, err := os.Stat(staleFile); !os.IsNotExist(err) {
		t.Fatalf("expected stale local sync state to be removed, got err=%v", err)
	}
}

func TestProcStatIsZombie(t *testing.T) {
	if !procStatIsZombie([]byte("123 (okdev) Z 1 2 3")) {
		t.Fatal("expected zombie proc stat to be detected")
	}
	if procStatIsZombie([]byte("123 (okdev) S 1 2 3")) {
		t.Fatal("did not expect running proc stat to be zombie")
	}
	if procStatIsZombie([]byte("malformed")) {
		t.Fatal("did not expect malformed proc stat to be zombie")
	}
}

func TestPSStatIsZombie(t *testing.T) {
	if !psStatIsZombie("Z+") {
		t.Fatal("expected ps zombie state to be detected")
	}
	if psStatIsZombie("Ss") {
		t.Fatal("did not expect normal ps state to be zombie")
	}
}

func TestReportSyncthingTargetReset(t *testing.T) {
	var out bytes.Buffer
	reportSyncthingTargetReset(&out, "sess-a", true)
	if got := out.String(); got != "Reset local sync state for session sess-a: target pod was recreated\n" {
		t.Fatalf("unexpected reset report %q", got)
	}

	out.Reset()
	reportSyncthingTargetReset(&out, "sess-a", false)
	if out.Len() != 0 {
		t.Fatalf("expected no output when reset is false, got %q", out.String())
	}
}
