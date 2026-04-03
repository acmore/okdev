package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestEffectiveSSHForwardAgent(t *testing.T) {
	cfg := &config.DevEnvironment{}
	if effectiveSSHForwardAgent(cfg, false, false) {
		t.Fatal("expected default forwardAgent to be disabled")
	}

	v := true
	cfg.Spec.SSH.ForwardAgent = &v
	if !effectiveSSHForwardAgent(cfg, false, false) {
		t.Fatal("expected config forwardAgent=true to enable forwarding")
	}
	if !effectiveSSHForwardAgent(cfg, true, false) {
		t.Fatal("expected flag override to enable forwarding")
	}
	if effectiveSSHForwardAgent(cfg, true, true) {
		t.Fatal("expected no-forward-agent to win")
	}
}

func TestResolveLocalSSHAgentSocket(t *testing.T) {
	t.Setenv("SSH_AUTH_SOCK", "")
	if _, err := resolveLocalSSHAgentSocket(); err == nil {
		t.Fatal("expected missing SSH_AUTH_SOCK error")
	}

	filePath := filepath.Join(t.TempDir(), "not-a-socket")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SSH_AUTH_SOCK", filePath)
	if _, err := resolveLocalSSHAgentSocket(); err == nil {
		t.Fatal("expected non-socket SSH_AUTH_SOCK error")
	}
}
