package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureEmbeddedSSHDRunning(t *testing.T) {
	fake := newFakeDevShellExecutor([]string{""}, nil)

	if err := ensureEmbeddedSSHDRunning(context.Background(), fake, "default", "okdev-test"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.container != "dev" {
		t.Fatalf("expected dev container, got %q", fake.container)
	}
	for _, want := range []string{
		"/var/okdev/okdev-sshd",
		"$HOME/.ssh/authorized_keys",
		`export OKDEV_TMUX="${OKDEV_TMUX:-0}"`,
		`export OKDEV_WORKSPACE="${OKDEV_WORKSPACE:-/workspace}"`,
		"nohup /var/okdev/okdev-sshd",
	} {
		if !strings.Contains(fake.lastScript, want) {
			t.Fatalf("expected embedded ssh start script to contain %q, got %q", want, fake.lastScript)
		}
	}
}

func TestEnsureEmbeddedSSHDRunningError(t *testing.T) {
	fake := newFakeDevShellExecutor(nil, []error{errors.New("boom")})

	err := ensureEmbeddedSSHDRunning(context.Background(), fake, "default", "okdev-test")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}
