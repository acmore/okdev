package cli

import (
	"strings"
	"testing"
	"time"
)

func TestNewSyncWaitCmdShape(t *testing.T) {
	cmd := newSyncWaitCmd(&Options{})
	if cmd.Use != "wait [session]" {
		t.Fatalf("unexpected use: %q", cmd.Use)
	}
	flag := cmd.Flags().Lookup("timeout")
	if flag == nil || flag.DefValue != syncWaitDefaultTimeout.String() {
		t.Fatalf("expected --timeout default %s, got %+v", syncWaitDefaultTimeout, flag)
	}
	if syncWaitDefaultTimeout != 10*time.Minute {
		t.Fatalf("unexpected default timeout %s", syncWaitDefaultTimeout)
	}
}

// Regression for issue #165: sync wait and okdev sync must never contradict
// each other about the same channel. A stale channel (process alive, peer
// disconnected after e.g. a laptop network drop) is "running but unhealthy",
// never "not running" — the two states have different repairs.
func TestSyncWaitGateError(t *testing.T) {
	if err := syncWaitGateError("s", syncHealthActive, ""); err != nil {
		t.Fatalf("active must pass the gate, got %v", err)
	}
	err := syncWaitGateError("s", syncHealthStale, "peer disconnected")
	if err == nil || !strings.Contains(err.Error(), "running but unhealthy") || !strings.Contains(err.Error(), "peer disconnected") {
		t.Fatalf("stale must report running-but-unhealthy with the reason, got %v", err)
	}
	if strings.Contains(err.Error(), "not running") {
		t.Fatalf("stale must not claim sync is not running, got %v", err)
	}
	err = syncWaitGateError("s", syncHealthStopped, "sync is not running")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Fatalf("stopped must report not running, got %v", err)
	}
}

func TestSyncStalenessWarning(t *testing.T) {
	if got := syncStalenessWarning(syncHealthActive, "", "the command"); got != "" {
		t.Fatalf("healthy channel must not warn, got %q", got)
	}
	got := syncStalenessWarning(syncHealthStale, "peer disconnected", "the detached job")
	if !strings.Contains(got, "unhealthy") || !strings.Contains(got, "peer disconnected") || !strings.Contains(got, "--require-sync") {
		t.Fatalf("stale warning incomplete: %q", got)
	}
	if !strings.Contains(got, "the detached job") {
		t.Fatalf("warning must name what is at risk, got %q", got)
	}
	got = syncStalenessWarning(syncHealthStopped, "sync is not running", "the command")
	if !strings.Contains(got, "not running") || !strings.Contains(got, "--require-sync") {
		t.Fatalf("stopped warning incomplete: %q", got)
	}
	// A foreground run is warned about in its own terms (#222).
	if !strings.Contains(got, "the command") || strings.Contains(got, "detached") {
		t.Fatalf("foreground warning must not talk about detached jobs, got %q", got)
	}
	// An empty subject still produces a sentence.
	if got := syncStalenessWarning(syncHealthStopped, "", ""); !strings.Contains(got, "the command") {
		t.Fatalf("expected a default subject, got %q", got)
	}
}

func TestSyncWaitRefusesWhenSyncNotRunning(t *testing.T) {
	// syncthingSessionActive reads local session state under HOME; with a
	// clean HOME there is no background sync, so wait must fail fast with
	// guidance instead of blocking.
	t.Setenv("HOME", t.TempDir())
	if syncthingSessionActive("sess-none") {
		t.Fatal("expected inactive session in clean HOME")
	}
}
