package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEnsureDevSSHDRunning(t *testing.T) {
	fake := newFakeDevShellExecutor([]string{""}, nil)

	if err := ensureDevSSHDRunning(context.Background(), fake, "default", "okdev-test"); err != nil {
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
			t.Fatalf("expected dev ssh start script to contain %q, got %q", want, fake.lastScript)
		}
	}
}

func TestEnsureDevSSHDRunningError(t *testing.T) {
	fake := newFakeDevShellExecutor(nil, []error{errors.New("boom")})

	err := ensureDevSSHDRunning(context.Background(), fake, "default", "okdev-test")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}
