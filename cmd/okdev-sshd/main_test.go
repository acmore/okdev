package main

import (
	"strings"
	"testing"
)

func TestBuildInteractiveLoginScriptIncludesDevTmuxBootstrap(t *testing.T) {
	script := buildInteractiveLoginScript(map[string]string{}, "/bin/bash", "/workspace", "1")

	for _, want := range []string{
		"cd '/workspace'",
		"/workspace/.okdev/post-attach.sh",
		"/var/okdev/dev.tmux.conf",
		"exec tmux new-session -A -s okdev",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected script to contain %q, got:\n%s", want, script)
		}
	}
}

func TestBuildInteractiveLoginScriptSkipsTmuxWhenDisabledForSession(t *testing.T) {
	script := buildInteractiveLoginScript(map[string]string{"OKDEV_NO_TMUX": "1"}, "/bin/sh", "/workspace", "1")
	if strings.Contains(script, "tmux") {
		t.Fatalf("expected tmux bootstrap to be skipped, got:\n%s", script)
	}
	if !strings.Contains(script, "exec '/bin/sh' -l") {
		t.Fatalf("expected shell fallback, got:\n%s", script)
	}
}
