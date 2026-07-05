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
	if cmd := newExecCmd(&Options{}); cmd.Use != "exec [session]" || cmd.Flags().Lookup("cmd") != nil || cmd.Flags().Lookup("shell") == nil || cmd.Flags().Lookup("script") == nil || cmd.Flags().Lookup("all") == nil || cmd.Flags().Lookup("workers") == nil || cmd.Flags().Lookup("group") == nil || cmd.Flags().Lookup("group").Value.Type() != "stringArray" || cmd.Flags().Lookup("sequential") == nil || cmd.Flags().Lookup("parallel") == nil || cmd.Flags().Lookup("no-tty") == nil || cmd.Flags().Lookup("pod") == nil || cmd.Flags().Lookup("role") == nil || cmd.Flags().Lookup("label") == nil || cmd.Flags().Lookup("exclude") == nil || cmd.Flags().Lookup("container") == nil || cmd.Flags().Lookup("detach") == nil || cmd.Flags().Lookup("timeout") == nil || cmd.Flags().Lookup("log-dir") == nil || cmd.Flags().Lookup("no-prefix") == nil || cmd.Flags().Lookup("fanout") == nil {
		t.Fatalf("unexpected exec command shape")
	}
	if cmd := newExecJobsCmd(&Options{}); cmd.Use != "exec-jobs [session]" || cmd.Flags().Lookup("pod") == nil || cmd.Flags().Lookup("role") == nil || cmd.Flags().Lookup("label") == nil || cmd.Flags().Lookup("exclude") == nil || cmd.Flags().Lookup("container") == nil || cmd.Flags().Lookup("fanout") == nil {
		t.Fatalf("unexpected exec-jobs command shape")
	}
	if cmd := newJobsCmd(&Options{}); cmd.Use != "jobs" || cmd.Short == "" {
		t.Fatalf("unexpected jobs command shape")
	} else {
		for _, sub := range []string{"list", "logs", "stop", "wait"} {
			if _, _, err := cmd.Find([]string{sub}); err != nil {
				t.Fatalf("expected jobs subcommand %q: %v", sub, err)
			}
		}
	}
	if cmd := newJobsLogsCmd(&Options{}); cmd.Use != "logs <job-id> [session]" || cmd.Flags().Lookup("pod") == nil || cmd.Flags().Lookup("role") == nil || cmd.Flags().Lookup("label") == nil || cmd.Flags().Lookup("exclude") == nil || cmd.Flags().Lookup("container") == nil || cmd.Flags().Lookup("fanout") == nil {
		t.Fatalf("unexpected jobs logs command shape")
	}
	if cmd := newPortForwardCmd(&Options{}); cmd.Use != "port-forward [session] <local:remote>..." || cmd.Short == "" || cmd.Flags().Lookup("pod") == nil || cmd.Flags().Lookup("role") == nil || cmd.Flags().Lookup("address") == nil || cmd.Flags().Lookup("ready-only") == nil {
		t.Fatalf("unexpected port-forward command shape")
	}
	if cmd := newPortsCmd(&Options{}); cmd.Use != "ports" || cmd.Short == "" || cmd.Flags().Lookup("dry-run") == nil {
		t.Fatalf("unexpected ports command shape")
	}
	if cmd := newAgentListCmd(&Options{}); cmd.Use != "list [session]" || cmd.Short == "" {
		t.Fatalf("unexpected agent list command shape")
	}
	if cmd := newCpCmd(&Options{}); cmd.Use != "cp [session] <src> <dst>" || cmd.Flags().Lookup("all") == nil || cmd.Flags().Lookup("pod") == nil || cmd.Flags().Lookup("role") == nil || cmd.Flags().Lookup("label") == nil || cmd.Flags().Lookup("exclude") == nil || cmd.Flags().Lookup("container") == nil || cmd.Flags().Lookup("fanout") == nil {
		t.Fatalf("unexpected cp command shape")
	}
}
