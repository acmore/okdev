package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/session"
)

func TestNewUseCmdSavesActiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	cmd := newUseCmd(&Options{})
	cmd.SetArgs([]string{"My_Session"})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("use execute: %v", err)
	}
	got, err := session.LoadActiveSession()
	if err != nil {
		t.Fatalf("load active session: %v", err)
	}
	if got != "my-session" {
		t.Fatalf("unexpected active session %q", got)
	}
	if !strings.Contains(out.String(), "Active session set to my-session") {
		t.Fatalf("unexpected output %q", out.String())
	}
}
