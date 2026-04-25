package cli

import "testing"

func TestNewRootCmdRegistersExpectedCommandsAndFlags(t *testing.T) {
	cmd := NewRootCmd()
	if cmd.Use != "okdev" {
		t.Fatalf("unexpected root use %q", cmd.Use)
	}
	if !cmd.SilenceUsage {
		t.Fatal("expected root command to suppress usage for runtime errors")
	}
	if !cmd.SilenceErrors {
		t.Fatal("expected root command to suppress cobra error printing")
	}
	for _, name := range []string{
		"version", "init", "validate", "up", "down", "status", "list", "use",
		"target", "agent", "exec", "logs", "ssh", "ssh-proxy", "ports", "port-forward", "sync", "prune", "migrate", "completion",
	} {
		if _, _, err := cmd.Find([]string{name}); err != nil {
			t.Fatalf("expected subcommand %q: %v", name, err)
		}
	}
	for _, flag := range []string{"config", "session", "owner", "namespace", "context", "output", "verbose"} {
		if got := cmd.PersistentFlags().Lookup(flag); got == nil {
			t.Fatalf("expected persistent flag %q", flag)
		}
	}
}
