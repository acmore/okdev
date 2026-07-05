package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewRestartCmdFlags(t *testing.T) {
	cmd := newRestartCmd(&Options{})
	if cmd.Use != "restart [session]" {
		t.Fatalf("unexpected use: %q", cmd.Use)
	}
	yes := cmd.Flags().Lookup("yes")
	if yes == nil || yes.Shorthand != "y" {
		t.Fatalf("expected --yes/-y flag, got %+v", yes)
	}
	wt := cmd.Flags().Lookup("wait-timeout")
	if wt == nil || wt.DefValue != upDefaultWaitTimeout.String() {
		t.Fatalf("expected --wait-timeout default %s, got %+v", upDefaultWaitTimeout, wt)
	}
}

func TestConfirmRestartRefusesNonInteractive(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmRestart(strings.NewReader("y\n"), &out, "sess", "default", "Pod", "okdev-sess")
	if err == nil || ok {
		t.Fatalf("expected non-interactive refusal, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error should point at --yes, got %v", err)
	}
}
