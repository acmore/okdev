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
		22,
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
	if !strings.Contains(text, "--session") || !strings.Contains(text, "test-session") || !strings.Contains(text, " -n ") || !strings.Contains(text, "dev-ns") || !strings.Contains(text, "ssh-proxy --remote-port 22") {
		t.Fatalf("proxy command missing namespace: %s", text)
	}
	if !strings.Contains(text, "LocalForward 8080 127.0.0.1:8080") {
		t.Fatalf("missing local forward: %s", text)
	}
}
