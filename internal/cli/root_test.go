package cli

import "testing"

func TestNewRootCmdRegistersExpectedCommandsAndFlags(t *testing.T) {
	cmd := NewRootCmd()
	if cmd.Use != "okdev" {
		t.Fatalf("unexpected root use %q", cmd.Use)
	}
	for _, name := range []string{
		"version", "init", "validate", "up", "down", "status", "list", "use",
		"target", "agent", "connect", "logs", "ssh", "ssh-proxy", "ports", "sync", "prune", "migrate",
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
