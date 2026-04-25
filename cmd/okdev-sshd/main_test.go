package main

import (
	"bytes"
	"errors"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

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
		"xterm-ghostty",
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

func TestBuildInteractiveLoginScriptWithoutWorkspaceOrTmux(t *testing.T) {
	script := buildInteractiveLoginScript(
		map[string]string{},
		"/bin/sh",
		"",
		"",
	)
	if strings.Contains(script, "post-attach.sh") {
		t.Fatalf("did not expect post-attach hook in script: %s", script)
	}
	if strings.Contains(script, "tmux") {
		t.Fatalf("did not expect tmux bootstrap in script: %s", script)
	}
	if !strings.Contains(script, "xterm-ghostty") {
		t.Fatalf("expected terminal bootstrap in script: %s", script)
	}
	if !strings.Contains(script, "exec '/bin/sh' -l") {
		t.Fatalf("expected login shell exec: %s", script)
	}
}

func TestDevTmuxBootstrapScriptIncludesFallbackWarning(t *testing.T) {
	script := devTmuxBootstrapScript()
	for _, want := range []string{
		"tmux",
		"warning: tmux not available",
		"set-environment -g SSH_AUTH_SOCK",
		"has-session -t okdev",
		"attach-session -t okdev",
		"source-file /var/okdev/dev.tmux.conf",
		`"${OKDEV_NESTED_TMUX:-0}" = "1"`,
		"set -g mouse off",
		"set -g mouse on",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected tmux bootstrap script to contain %q: %s", want, script)
		}
	}
}

func TestTerminalBootstrapScriptNormalizesGhostty(t *testing.T) {
	script := terminalBootstrapScript()
	for _, want := range []string{"xterm-ghostty", "xterm-256color"} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected terminal bootstrap to contain %q: %s", want, script)
		}
	}
}

type fakeSessionEnv struct {
	ssh.Session
	env []string
}

func (s fakeSessionEnv) Environ() []string { return s.env }

type fakeSessionCmd struct {
	ssh.Session
	raw string
	env []string
}

func (s fakeSessionCmd) RawCommand() string { return s.raw }
func (s fakeSessionCmd) Environ() []string  { return s.env }

func TestSessionEnvMap(t *testing.T) {
	env := sessionEnvMap(fakeSessionEnv{env: []string{
		"FOO=bar",
		"EMPTY=",
		"INVALID",
	}})
	if env["FOO"] != "bar" {
		t.Fatalf("expected FOO env, got %#v", env)
	}
	if v, ok := env["EMPTY"]; !ok || v != "" {
		t.Fatalf("expected EMPTY env, got %#v", env)
	}
	if _, ok := env["INVALID"]; ok {
		t.Fatalf("did not expect malformed env entry, got %#v", env)
	}
}

func TestBuildCmdInteractiveShell(t *testing.T) {
	t.Setenv("OKDEV_WORKSPACE", "")
	t.Setenv("OKDEV_TMUX", "")
	cmd := buildCmd(fakeSessionCmd{}, "/bin/sh", nil)
	want := `/bin/sh -lc if [ "${TERM:-}" = "xterm-ghostty" ]; then export TERM=xterm-256color; fi; exec '/bin/sh' -l`
	if got := strings.Join(cmd.Args, " "); got != want {
		t.Fatalf("unexpected interactive args: %q", got)
	}
}

func TestBuildCmdRawCommandUsesLoginShellCommandMode(t *testing.T) {
	cmd := buildCmd(fakeSessionCmd{raw: "echo hi", env: []string{"A=B"}}, "/bin/bash", []string{"SSH_AUTH_SOCK=/tmp/agent.sock"})
	if got := strings.Join(cmd.Args, " "); got != "/bin/bash -lc echo hi" {
		t.Fatalf("unexpected raw command args: %q", got)
	}
	if !contains(cmd.Env, "A=B") {
		t.Fatalf("expected session env to be appended: %#v", cmd.Env)
	}
	if !contains(cmd.Env, "SSH_AUTH_SOCK=/tmp/agent.sock") {
		t.Fatalf("expected extra env to be appended: %#v", cmd.Env)
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

func TestExitStatusSignalReturnsNonZero(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "kill -TERM $$")
	err := cmd.Run()
	if err == nil {
		t.Fatal("expected signal exit error")
	}
	if got := exitStatus(err); got == 0 {
		t.Fatalf("expected signal exit to be non-zero, got %d", got)
	}
}

func contains(items []string, want string) bool {
	for _, item := range items {
		if item == want {
			return true
		}
	}
	return false
}

type fakeSessionRuntime struct {
	raw       string
	env       []string
	stdin     *bytes.Buffer
	stdout    bytes.Buffer
	stderr    bytes.Buffer
	exitCode  int
	exitCalls int
	pty       ssh.Pty
	winCh     chan ssh.Window
	isPty     bool
}

func newFakeSessionRuntime(raw string, input string) *fakeSessionRuntime {
	return &fakeSessionRuntime{
		raw:   raw,
		stdin: bytes.NewBufferString(input),
		winCh: make(chan ssh.Window),
	}
}

func (s *fakeSessionRuntime) Read(p []byte) (int, error)  { return s.stdin.Read(p) }
func (s *fakeSessionRuntime) Write(p []byte) (int, error) { return s.stdout.Write(p) }
func (s *fakeSessionRuntime) Close() error                { return nil }
func (s *fakeSessionRuntime) CloseWrite() error           { return nil }
func (s *fakeSessionRuntime) SendRequest(string, bool, []byte) (bool, error) {
	return false, nil
}
func (s *fakeSessionRuntime) Stderr() io.ReadWriter        { return &s.stderr }
func (s *fakeSessionRuntime) User() string                 { return "dev" }
func (s *fakeSessionRuntime) RemoteAddr() net.Addr         { return dummyAddr("remote") }
func (s *fakeSessionRuntime) LocalAddr() net.Addr          { return dummyAddr("local") }
func (s *fakeSessionRuntime) Environ() []string            { return s.env }
func (s *fakeSessionRuntime) Exit(code int) error          { s.exitCode = code; s.exitCalls++; return nil }
func (s *fakeSessionRuntime) Command() []string            { return nil }
func (s *fakeSessionRuntime) RawCommand() string           { return s.raw }
func (s *fakeSessionRuntime) Subsystem() string            { return "" }
func (s *fakeSessionRuntime) PublicKey() ssh.PublicKey     { return nil }
func (s *fakeSessionRuntime) Context() ssh.Context         { return nil }
func (s *fakeSessionRuntime) Permissions() ssh.Permissions { return ssh.Permissions{} }
func (s *fakeSessionRuntime) Pty() (ssh.Pty, <-chan ssh.Window, bool) {
	return s.pty, s.winCh, s.isPty
}
func (s *fakeSessionRuntime) Signals(chan<- ssh.Signal) {}
func (s *fakeSessionRuntime) Break(chan<- bool)         {}

type dummyAddr string

func (a dummyAddr) Network() string { return "tcp" }
func (a dummyAddr) String() string  { return string(a) }

func TestHandleNoPTYCopiesStdoutStderrAndStdin(t *testing.T) {
	sess := newFakeSessionRuntime("", "hello\n")
	cmd := exec.Command("/bin/sh", "-c", "read line; echo out:$line; echo err:$line >&2")

	if err := handleNoPTY(cmd, sess); err != nil {
		t.Fatalf("handleNoPTY: %v", err)
	}
	if got := sess.stdout.String(); !strings.Contains(got, "out:hello") {
		t.Fatalf("expected stdout to contain command output, got %q", got)
	}
	if got := sess.stderr.String(); !strings.Contains(got, "err:hello") {
		t.Fatalf("expected stderr to contain command output, got %q", got)
	}
}

func TestHandlePTYExitsWhenSessionInputCloses(t *testing.T) {
	sess := newFakeSessionRuntime("", "")
	sess.isPty = true
	sess.pty = ssh.Pty{Term: "xterm-256color"}

	done := make(chan error, 1)
	go func() {
		done <- handlePTY(exec.Command("/bin/cat"), sess, sess.pty, sess.winCh)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handlePTY did not exit after session input closed")
	}
}

func TestSessionHandlerExecCommandExitsWithCommandStatus(t *testing.T) {
	sess := newFakeSessionRuntime("printf hello; printf oops >&2; exit 7", "")

	sessionHandler("/bin/sh")(sess)

	if sess.exitCalls != 1 {
		t.Fatalf("expected one exit call, got %d", sess.exitCalls)
	}
	if sess.exitCode != 7 {
		t.Fatalf("expected exit status 7, got %d", sess.exitCode)
	}
	if got := sess.stdout.String(); !strings.Contains(got, "hello") {
		t.Fatalf("expected stdout to contain hello, got %q", got)
	}
	if got := sess.stderr.String(); !strings.Contains(got, "oops") {
		t.Fatalf("expected stderr to contain oops, got %q", got)
	}
}

func TestSessionHandlerPTYStartFailureReturnsExitCode(t *testing.T) {
	sess := newFakeSessionRuntime("", "")
	sess.isPty = true
	sess.pty = ssh.Pty{Term: "xterm-256color"}

	sessionHandler("/definitely/missing/shell")(sess)

	if sess.exitCalls != 1 {
		t.Fatalf("expected one exit call, got %d", sess.exitCalls)
	}
	if sess.exitCode == 0 {
		t.Fatalf("expected non-zero exit code for PTY start failure, got %d", sess.exitCode)
	}
}

func TestSetWinsizeDoesNotPanic(t *testing.T) {
	f, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		t.Fatalf("open %s: %v", os.DevNull, err)
	}
	defer f.Close()

	setWinsize(f, 120, 40)
}
