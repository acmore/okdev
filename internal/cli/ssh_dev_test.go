package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestEnsureDevSSHDRunning(t *testing.T) {
	fake := newFakeDevShellExecutor([]string{""}, nil)

	if err := ensureDevSSHDRunning(context.Background(), fake, "default", "okdev-test", "trainer"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if fake.container != "trainer" {
		t.Fatalf("expected trainer container, got %q", fake.container)
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

	err := ensureDevSSHDRunning(context.Background(), fake, "default", "okdev-test", "trainer")
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected error containing boom, got %v", err)
	}
}

// fakeShellRunner implements sshShellRunner for testing reconnection logic.
type fakeShellRunner struct {
	shellErrors []error
	shellCalls  atomic.Int32
	connected   atomic.Bool
	reconnected atomic.Int32
	execErr     error
}

func (f *fakeShellRunner) OpenShell() error {
	idx := int(f.shellCalls.Add(1)) - 1
	if idx < len(f.shellErrors) {
		return f.shellErrors[idx]
	}
	return nil
}

func (f *fakeShellRunner) IsConnected() bool { return f.connected.Load() }

func (f *fakeShellRunner) WaitConnected(ctx context.Context) bool {
	f.connected.Store(true)
	return true
}

func (f *fakeShellRunner) ForceReconnect() {
	f.reconnected.Add(1)
	f.connected.Store(false)
}

func (f *fakeShellRunner) ExecContext(_ context.Context, _ string) ([]byte, error) {
	return nil, f.execErr
}

func TestRunSSHShellWithReconnectCleanExit(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{connected: atomic.Bool{}}
	runner.connected.Store(true)

	var errOut bytes.Buffer
	err := runSSHShellWithReconnect(context.Background(), runner, "test-sess", &errOut)
	if err != nil {
		t.Fatalf("expected clean exit, got %v", err)
	}
	if runner.shellCalls.Load() != 1 {
		t.Fatalf("expected 1 shell call, got %d", runner.shellCalls.Load())
	}
}

func TestRunSSHShellWithReconnectNonRecoverableError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("permission denied")},
	}
	runner.connected.Store(true)

	var errOut bytes.Buffer
	err := runSSHShellWithReconnect(context.Background(), runner, "test-sess", &errOut)
	if err == nil || !strings.Contains(err.Error(), "ssh shell failed") {
		t.Fatalf("expected ssh shell failed error, got %v", err)
	}
}

func TestRunSSHShellWithReconnectRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("connection reset by peer"), nil},
	}
	runner.connected.Store(true)

	var errOut bytes.Buffer
	err := runSSHShellWithReconnect(context.Background(), runner, "test-sess", &errOut)
	if err != nil {
		t.Fatalf("expected successful reconnect, got %v", err)
	}
	if runner.shellCalls.Load() != 2 {
		t.Fatalf("expected 2 shell calls, got %d", runner.shellCalls.Load())
	}
	if !strings.Contains(errOut.String(), "Connection lost") {
		t.Fatalf("expected 'Connection lost' message, got %q", errOut.String())
	}
}

func TestRunSSHShellWithReconnectContextCancelled(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	ctx, cancel := context.WithCancel(context.Background())

	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("EOF")},
	}
	runner.connected.Store(false)
	origWait := runner.WaitConnected
	_ = origWait
	// Override WaitConnected to cancel context to simulate user interrupt
	cancel()

	var errOut bytes.Buffer
	err := runSSHShellWithReconnect(ctx, runner, "test-sess", &errOut)
	if err != nil {
		t.Fatalf("expected nil on context cancel, got %v", err)
	}
}

func TestRunSSHShellWithReconnectRapidFailures(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	// Generate enough rapid failures to trigger the circuit breaker
	errs := make([]error, rapidFailureThreshold+1)
	for i := range errs {
		errs[i] = errors.New("connection reset by peer")
	}
	runner := &fakeShellRunner{
		shellErrors: errs,
	}
	runner.connected.Store(true)

	var errOut bytes.Buffer
	err := runSSHShellWithReconnect(context.Background(), runner, "test-sess", &errOut)
	if err == nil || !strings.Contains(err.Error(), "failed repeatedly") {
		t.Fatalf("expected rapid failure circuit breaker, got %v", err)
	}
}

func TestRunSSHShellWithReconnectDetectsContainerRestart(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("connection reset by peer")},
	}
	runner.connected.Store(true)

	checkRestart := func() *kube.ContainerRestartInfo {
		return &kube.ContainerRestartInfo{
			RestartCount: 2,
			LastReason:   "OOMKilled",
		}
	}

	var errOut bytes.Buffer
	err := runSSHShellWithReconnectAndRestartCheck(context.Background(), runner, "test-sess", &errOut, checkRestart)
	if err != nil {
		t.Fatalf("expected nil return on container restart, got %v", err)
	}
	output := errOut.String()
	if !strings.Contains(output, "OOMKilled") {
		t.Fatalf("expected OOMKilled in output, got %q", output)
	}
	if !strings.Contains(output, "Container restarted") {
		t.Fatalf("expected 'Container restarted' in output, got %q", output)
	}
}

func TestRunSSHShellWithReconnectNoRestartContinuesRecovery(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("connection reset by peer"), nil},
	}
	runner.connected.Store(true)

	// checkRestart returns nil — no restart detected, should continue reconnecting.
	checkRestart := func() *kube.ContainerRestartInfo {
		return nil
	}

	var errOut bytes.Buffer
	err := runSSHShellWithReconnectAndRestartCheck(context.Background(), runner, "test-sess", &errOut, checkRestart)
	if err != nil {
		t.Fatalf("expected successful reconnect, got %v", err)
	}
	if !strings.Contains(errOut.String(), "Connection lost") {
		t.Fatalf("expected 'Connection lost' message, got %q", errOut.String())
	}
}

func TestRunSSHShellWithReconnectDetectsContainerCrashLoop(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	runner := &fakeShellRunner{
		shellErrors: []error{errors.New("connection reset by peer")},
	}
	runner.connected.Store(true)

	checkRestart := func() *kube.ContainerRestartInfo {
		return &kube.ContainerRestartInfo{
			RestartCount:   5,
			LastReason:     "Error",
			LastExitCode:   137,
			CurrentWaiting: "CrashLoopBackOff",
		}
	}

	var errOut bytes.Buffer
	err := runSSHShellWithReconnectAndRestartCheck(context.Background(), runner, "test-sess", &errOut, checkRestart)
	if err != nil {
		t.Fatalf("expected nil return on container restart, got %v", err)
	}
	output := errOut.String()
	if !strings.Contains(output, "CrashLoopBackOff") {
		t.Fatalf("expected CrashLoopBackOff in output, got %q", output)
	}
}

func TestPrintContainerRestartNotice(t *testing.T) {
	tests := []struct {
		name     string
		info     kube.ContainerRestartInfo
		contains []string
	}{
		{
			name:     "OOMKilled",
			info:     kube.ContainerRestartInfo{RestartCount: 1, LastReason: "OOMKilled"},
			contains: []string{"Container restarted (OOMKilled)", "tmux sessions"},
		},
		{
			name:     "CrashLoopBackOff",
			info:     kube.ContainerRestartInfo{RestartCount: 5, LastReason: "Error", CurrentWaiting: "CrashLoopBackOff"},
			contains: []string{"Container restarted (Error)", "CrashLoopBackOff"},
		},
		{
			name:     "unknown reason",
			info:     kube.ContainerRestartInfo{RestartCount: 1},
			contains: []string{"Container restarted (unknown)"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var buf bytes.Buffer
			printContainerRestartNotice(&buf, &tt.info)
			for _, want := range tt.contains {
				if !strings.Contains(buf.String(), want) {
					t.Errorf("expected output to contain %q, got %q", want, buf.String())
				}
			}
		})
	}
}
