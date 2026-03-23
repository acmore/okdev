package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestEnsureSSHConfigEntryIncludesNamespaceInProxyCommand(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	_, err := ensureSSHConfigEntry(
		"okdev-test",
		"test-session",
		"dev-ns",
		"root",
		2222,
		"/tmp/id_ed25519",
		"/tmp/.okdev.yaml",
		[]config.PortMapping{{Local: 8080, Remote: 8080}},
	)
	if err != nil {
		t.Fatalf("ensureSSHConfigEntry: %v", err)
	}

	cfgPath := filepath.Join(home, ".ssh", "config")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "Host okdev-test") {
		t.Fatalf("missing host block: %s", text)
	}
	if !strings.Contains(text, "--session") || !strings.Contains(text, "test-session") || !strings.Contains(text, " -n ") || !strings.Contains(text, "dev-ns") || !strings.Contains(text, "ssh-proxy --remote-port 2222") {
		t.Fatalf("proxy command missing namespace: %s", text)
	}
	if strings.Contains(text, "LocalForward") {
		t.Fatalf("interactive ssh host entry must not include LocalForward directives: %s", text)
	}
}

func TestEnsureSSHConfigEntryUsesProxyKeepaliveValues(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	// Keepalive values are hardcoded — verify they're 5/10 regardless of config defaults
	_, err := ensureSSHConfigEntry(
		"okdev-test2",
		"test-session2",
		"dev-ns",
		"root",
		2222,
		"/tmp/id_ed25519",
		"/tmp/.okdev.yaml",
		nil,
	)
	if err != nil {
		t.Fatalf("ensureSSHConfigEntry: %v", err)
	}

	cfgPath := filepath.Join(home, ".ssh", "config")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "ServerAliveInterval 5") {
		t.Fatalf("expected ServerAliveInterval 5, got: %s", text)
	}
	if !strings.Contains(text, "ServerAliveCountMax 10") {
		t.Fatalf("expected ServerAliveCountMax 10, got: %s", text)
	}
}
