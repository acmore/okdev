package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
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
	if !strings.Contains(text, "SetEnv OKDEV_NO_TMUX=1") {
		t.Fatalf("expected managed ssh host to disable tmux by default: %s", text)
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
	if _, err := os.Stat(cfgPath + ".okdev.lock"); !os.IsNotExist(err) {
		t.Fatalf("expected ssh config lock file to be cleaned up, got err=%v", err)
	}
}

func TestManagedSSHForwardArgs(t *testing.T) {
	args := managedSSHForwardArgs(
		"okdev-test",
		"/tmp/ssh-config",
		"/tmp/okdev.sock",
		[]config.PortMapping{
			{Name: "app", Local: 8080, Remote: 80},
			{Name: "hybrid", Local: 3000, Remote: 3000, Direction: config.PortDirectionReverse},
		},
		config.SSHSpec{KeepAliveInterval: 10, KeepAliveCountMax: 30},
	)

	want := []string{
		"ssh",
		"-F", "/tmp/ssh-config",
		"-fN",
		"-M",
		"-S", "/tmp/okdev.sock",
		"-o", "ControlPersist=3600",
		"-o", "ExitOnForwardFailure=no",
		"-o", "ServerAliveInterval=10",
		"-o", "ServerAliveCountMax=30",
		"-o", "TCPKeepAlive=yes",
		"-o", "LogLevel=ERROR",
		"-L", "8080:127.0.0.1:80",
		"-R", "3000:127.0.0.1:3000",
		"okdev-test",
	}
	if !reflect.DeepEqual(args, want) {
		t.Fatalf("unexpected args:\n got: %#v\nwant: %#v", args, want)
	}
}

func TestStartManagedSSHForwardWithForwardsRemovesStaleSocketAfterFailedCheck(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	origExecCommand := execCommand
	t.Cleanup(func() {
		execCommand = origExecCommand
	})

	socketPath := filepath.Join(home, ".okdev", "ssh", "okdev-test.sock")
	if err := os.MkdirAll(filepath.Dir(socketPath), 0o700); err != nil {
		t.Fatalf("mkdir socket dir: %v", err)
	}
	if err := os.WriteFile(socketPath, []byte("stale"), 0o600); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}

	var calls [][]string
	execCommand = func(name string, args ...string) *exec.Cmd {
		calls = append(calls, append([]string{name}, args...))
		if len(args) >= 4 && args[2] == "-O" && args[3] == "check" {
			return exec.Command("sh", "-c", "exit 1")
		}
		cmd := exec.Command("sh", "-c", `[ ! -e "$SOCKET_PATH" ]`)
		cmd.Env = append(os.Environ(), "SOCKET_PATH="+socketPath)
		return cmd
	}

	if err := startManagedSSHForwardWithForwards("okdev-test", nil, config.SSHSpec{}); err != nil {
		t.Fatalf("startManagedSSHForwardWithForwards: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected check and start calls, got %d", len(calls))
	}
	if _, err := os.Stat(socketPath); !os.IsNotExist(err) {
		t.Fatalf("expected stale socket to be removed, got err=%v", err)
	}
}
