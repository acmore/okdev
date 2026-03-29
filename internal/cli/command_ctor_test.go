package cli

import (
	"io"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestResolveSessionNameHelpersWithExplicitSession(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.DefaultNameTemplate = "fallback"

	got, err := resolveSessionName(&Options{Session: "sess-a"}, cfg, "default")
	if err != nil {
		t.Fatalf("resolveSessionName: %v", err)
	}
	if got != "sess-a" {
		t.Fatalf("unexpected session %q", got)
	}

	got, err = resolveSessionNameWithState(&Options{Session: "sess-b"}, cfg, "default", true)
	if err != nil {
		t.Fatalf("resolveSessionNameWithState: %v", err)
	}
	if got != "sess-b" {
		t.Fatalf("unexpected session %q", got)
	}
}

func TestStartSessionMaintenanceDisabledIsNoop(t *testing.T) {
	stop := startSessionMaintenance(&Options{}, "default", "sess-a", io.Discard, false)
	stop()
}

func TestCommandConstructorsExposeExpectedMetadata(t *testing.T) {
	if cmd := newConnectCmd(&Options{}); cmd.Use != "connect [session]" || cmd.Flags().Lookup("cmd") == nil || cmd.Flags().Lookup("shell") == nil || cmd.Flags().Lookup("no-tty") == nil {
		t.Fatalf("unexpected connect command shape")
	}
	if cmd := newPortsCmd(&Options{}); cmd.Use != "ports" || cmd.Short == "" || cmd.Flags().Lookup("dry-run") == nil {
		t.Fatalf("unexpected ports command shape")
	}
	if cmd := newAgentListCmd(&Options{}); cmd.Use != "list [session]" || cmd.Short == "" {
		t.Fatalf("unexpected agent list command shape")
	}
}
