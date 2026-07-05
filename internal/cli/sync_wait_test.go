package cli

import (
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

func TestSyncWaitRefusesWhenSyncNotRunning(t *testing.T) {
	// syncthingSessionActive reads local session state under HOME; with a
	// clean HOME there is no background sync, so wait must fail fast with
	// guidance instead of blocking.
	t.Setenv("HOME", t.TempDir())
	if syncthingSessionActive("sess-none") {
		t.Fatal("expected inactive session in clean HOME")
	}
}
