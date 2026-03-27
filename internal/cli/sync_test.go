package cli

import (
	"os"
	"path/filepath"
	"testing"
)

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
