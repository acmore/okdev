package main

import (
	"errors"
	"os"
	"os/exec"
	"strings"
	"testing"

	"github.com/gliderlabs/ssh"
)

func TestNewServerEnablesLocalPortForwarding(t *testing.T) {
	srv := newServer(":2222", "/bin/sh", nil)

	if srv.ChannelHandlers == nil {
		t.Fatal("expected channel handlers to be configured")
	}
	if _, ok := srv.ChannelHandlers["session"]; !ok {
		t.Fatal("expected default session channel handler")
	}
	if _, ok := srv.ChannelHandlers["direct-tcpip"]; !ok {
		t.Fatal("expected direct-tcpip channel handler for ssh forwarding")
	}
	if srv.LocalPortForwardingCallback == nil {
		t.Fatal("expected local port forwarding callback")
	}
	if !srv.LocalPortForwardingCallback(nil, "127.0.0.1", 8080) {
		t.Fatal("expected local port forwarding to be allowed")
	}
}

func TestNewServerAddsPublicKeyHandlerWhenKeysProvided(t *testing.T) {
	pub, _, _, _, err := ssh.ParseAuthorizedKey([]byte("ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE9mN6e2Q2x8tQz4pT2r8j04YfGLwRoTSesFiNUFDXL9 test\n"))
	if err != nil {
		t.Fatalf("parse authorized key: %v", err)
	}

	srv := newServer(":2222", "/bin/sh", []ssh.PublicKey{pub})
	if srv.PublicKeyHandler == nil {
		t.Fatal("expected public key handler to be configured")
	}
}

func TestDetectShellReturnsExistingShell(t *testing.T) {
	got := detectShell()
	if got != "/bin/bash" && got != "/bin/sh" {
		t.Fatalf("unexpected shell %q", got)
	}
	if _, err := os.Stat(got); err != nil {
		t.Fatalf("expected detected shell to exist: %v", err)
	}
}

func TestLoadAuthorizedKeysMissingFileReturnsNil(t *testing.T) {
	keys, err := loadAuthorizedKeys("/definitely/missing/authorized_keys")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if keys != nil {
		t.Fatalf("expected nil keys for missing file, got %d", len(keys))
	}
}

func TestLoadAuthorizedKeysParsesMultipleKeys(t *testing.T) {
	path := t.TempDir() + "/authorized_keys"
	content := strings.Join([]string{
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIE9mN6e2Q2x8tQz4pT2r8j04YfGLwRoTSesFiNUFDXL9 test1",
		"ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIJ2uL4N6OQ9bQG6tW1c2GqU2o6L1Qm0f0g5d2sWlY4Hn test2",
		"",
	}, "\n")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	keys, err := loadAuthorizedKeys(path)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(keys) != 2 {
		t.Fatalf("unexpected key count: got %d want 2", len(keys))
	}
}

func TestLoadAuthorizedKeysRejectsInvalidData(t *testing.T) {
	path := t.TempDir() + "/authorized_keys"
	if err := os.WriteFile(path, []byte("not a key\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := loadAuthorizedKeys(path); err == nil {
		t.Fatal("expected parse error")
	}
}

func TestBuildInteractiveLoginScript(t *testing.T) {
	script := buildInteractiveLoginScript(
		map[string]string{},
		"/bin/sh",
		"/workspace/demo",
		"1",
	)

	for _, want := range []string{
		"cd '/workspace/demo'",
		"/workspace/demo/.okdev/post-attach.sh",
		"tmux",
		"exec '/bin/sh' -l",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected script to contain %q: %s", want, script)
		}
	}
}

func TestBuildInteractiveLoginScriptSkipsTmuxWhenDisabled(t *testing.T) {
	script := buildInteractiveLoginScript(
		map[string]string{"OKDEV_NO_TMUX": "1"},
		"/bin/bash",
		"",
		"1",
	)

	if strings.Contains(script, "tmux") {
		t.Fatalf("expected tmux bootstrap to be skipped: %s", script)
	}
	if !strings.Contains(script, "exec '/bin/bash' -l") {
		t.Fatalf("expected login shell exec: %s", script)
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("unexpected quoted string: %s", got)
	}
	if got := shellQuote(""); got != "''" {
		t.Fatalf("unexpected empty quoted string: %s", got)
	}
}

func TestExitStatus(t *testing.T) {
	if got := exitStatus(nil); got != 0 {
		t.Fatalf("unexpected nil exit status: %d", got)
	}

	if got := exitStatus(errors.New("plain")); got != 1 {
		t.Fatalf("unexpected generic exit status: %d", got)
	}

	cmd := exec.Command("/bin/sh", "-c", "exit 7")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected exit error")
	}
	if got := exitStatus(err); got != 7 {
		t.Fatalf("unexpected exit status: got %d want 7", got)
	}
}
