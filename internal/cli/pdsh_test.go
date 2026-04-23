package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	k8sexec "k8s.io/client-go/util/exec"
)

func TestFilterPodsByRole(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "master-0", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "worker-1", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}
	got := filterPodsByRole(pods, "worker")
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-1" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestFilterPodsByLabels(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Labels: map[string]string{"gpu": "a100", "zone": "us"}},
		{Name: "b", Labels: map[string]string{"gpu": "a100", "zone": "eu"}},
		{Name: "c", Labels: map[string]string{"gpu": "h100", "zone": "us"}},
	}
	got := filterPodsByLabels(pods, []string{"gpu=a100", "zone=us"})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected pod a, got %v", got)
	}
}

func TestFilterPodsByName(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
		{Name: "worker-1"},
		{Name: "master-0"},
	}
	got := filterPodsByName(pods, []string{"worker-0", "master-0"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
}

func TestFilterPodsByNameMissing(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
	}
	got := filterPodsByName(pods, []string{"worker-0", "no-such-pod"})
	if len(got) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(got))
	}
}

func TestExcludePods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
		{Name: "worker-1"},
		{Name: "worker-2"},
	}
	got := excludePods(pods, []string{"worker-1"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-2" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestDetachCommand(t *testing.T) {
	got := detachCommand("python train.py --epochs 100")
	expected := []string{"sh", "-c", "nohup sh -c 'python train.py --epochs 100' >/dev/null 2>&1 &"}
	if len(got) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(got), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("arg %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func TestDetachCommandWithQuotes(t *testing.T) {
	got := detachCommand("echo 'hello world'")
	expected := []string{"sh", "-c", "nohup sh -c 'echo '\\''hello world'\\''' >/dev/null 2>&1 &"}
	if len(got) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(got), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("arg %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func TestFilterRunningPods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Phase: "Running"},
		{Name: "b", Phase: "Pending"},
		{Name: "c", Phase: "Running"},
	}
	got := filterRunningPods(pods)
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
}

type fakePdshExecClient struct {
	mu      sync.Mutex
	calls   []string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakePdshExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, pod)
	output := f.outputs[pod]
	err := f.errs[pod]
	f.mu.Unlock()
	if output != "" {
		fmt.Fprint(stdout, output)
	}
	return err
}

func (f *fakePdshExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return f.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

type slowExecClient struct {
	delay time.Duration
}

func (s *slowExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return nil
	}
}

func (s *slowExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return s.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

func TestRunDetachExec(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{},
		errs:    map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", "python train.py", pdshDefaultFanout, &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "worker-0] detached") || !strings.Contains(stdout.String(), "worker-1] detached") {
		t.Fatalf("expected detach confirmation, got %q", stdout.String())
	}
}

func TestRunDetachExecWithError(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("pod not ready"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", "python train.py", pdshDefaultFanout, &stdout)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if !strings.Contains(stdout.String(), "worker-0] detached") {
		t.Fatalf("expected detach for worker-0, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "worker-1] error:") {
		t.Fatalf("expected error for worker-1, got %q", stdout.String())
	}
}

func TestRunMultiExecSuccess(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
			"okdev-sess-worker-1": "ok\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "worker-0] ok") || !strings.Contains(stdout.String(), "worker-1] ok") {
		t.Fatalf("expected prefixed output, got %q", stdout.String())
	}
}

func TestRunMultiExecPartialFailure(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": k8sexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1},
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	got := stderr.String()
	if !strings.Contains(got, "\nFAILED:\n") {
		t.Fatalf("expected blank line before multiline failure header, got %q", got)
	}
	if !strings.Contains(got, "pod=okdev-sess-worker-1 container=dev kind=remote-exit exit=1") {
		t.Fatalf("expected classified failure summary for worker-1, got %q", got)
	}
}

func TestRunMultiExecClassifiesTimeoutAndTransportFailures(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{},
		errs: map[string]error{
			"okdev-sess-worker-0": context.DeadlineExceeded,
			"okdev-sess-worker-1": errors.New("stream error: INTERNAL_ERROR"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "serving", []string{"sh", "-lc", "echo ok"}, "", 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for classified failures")
	}
	got := stderr.String()
	if !strings.Contains(got, "pod=okdev-sess-worker-0 container=serving kind=timeout") {
		t.Fatalf("expected timeout classification, got %q", got)
	}
	if !strings.Contains(got, "pod=okdev-sess-worker-1 container=serving kind=transport") {
		t.Fatalf("expected transport classification, got %q", got)
	}
}

func TestRunMultiExecTimeout(t *testing.T) {
	slowClient := &slowExecClient{delay: 5 * time.Second}
	pods := []kube.PodSummary{
		{Name: "pod-0", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), slowClient, "default", pods, "dev", []string{"sh", "-c", "sleep 100"}, "", 50*time.Millisecond, false, pdshDefaultFanout, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunMultiExecNoPrefix(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"pod-0": "hello\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "pod-0", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-c", "echo hello"}, "", 0, true, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got != "hello\n" {
		t.Fatalf("expected raw output, got %q", got)
	}
}

func TestRunMultiExecWithLogDir(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "gpu0\n",
			"okdev-sess-worker-1": "gpu1\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	logDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-c", "echo gpu"}, logDir, 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"worker-0.log", "worker-1.log"} {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

func TestValidateMultiPodFlags(t *testing.T) {
	tests := []struct {
		name     string
		podNames []string
		role     string
		labels   []string
		exclude  []string
		hasCmd   bool
		detach   bool
		wantErr  string
	}{
		{
			name:    "role requires command",
			role:    "worker",
			wantErr: "command after -- is required",
		},
		{
			name:    "detach requires cmd",
			detach:  true,
			wantErr: "command after -- is required",
		},
		{
			name:    "exclude without selector",
			exclude: []string{"p1"},
			wantErr: "command after -- is required",
		},
		{
			name:     "exclude with pod and command",
			podNames: []string{"p1"}, exclude: []string{"p1"}, hasCmd: true,
			wantErr: "--exclude cannot be used with --pod",
		},
		{
			name:   "valid default all with command",
			hasCmd: true,
		},
		{
			name: "valid role with cmd",
			role: "worker", hasCmd: true,
		},
		{
			name:    "valid exclude with default all",
			exclude: []string{"p1"}, hasCmd: true,
		},
		{
			name:   "valid detach with cmd",
			detach: true, hasCmd: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMultiPodFlags(tt.podNames, tt.role, tt.labels, tt.exclude, tt.hasCmd, tt.detach)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestFilterRunningPodsReadinessCheck(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "leader-0", Phase: "Running"},
		{Name: "worker-0", Phase: "Pending"},
		{Name: "worker-1", Phase: "ContainerCreating"},
	}
	running := filterRunningPods(allPods)

	// Without --ready-only, should detect the gap.
	if len(running) == len(allPods) {
		t.Fatal("expected fewer running pods than total")
	}
	if len(running) != 1 {
		t.Fatalf("expected 1 running pod, got %d", len(running))
	}
	if running[0].Name != "leader-0" {
		t.Fatalf("expected leader-0, got %s", running[0].Name)
	}
}
