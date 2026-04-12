package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
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
	err := runDetachExec(context.Background(), client, "default", pods, "dev", "python train.py", &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "worker-0: detached") || !strings.Contains(stdout.String(), "worker-1: detached") {
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
	err := runDetachExec(context.Background(), client, "default", pods, "dev", "python train.py", &stdout)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if !strings.Contains(stdout.String(), "worker-0: detached") {
		t.Fatalf("expected detach for worker-0, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "worker-1: error:") {
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
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "worker-0: ok") || !strings.Contains(stdout.String(), "worker-1: ok") {
		t.Fatalf("expected prefixed output, got %q", stdout.String())
	}
}

func TestRunMultiExecPartialFailure(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("exit code 1"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if !strings.Contains(stderr.String(), "worker-1") {
		t.Fatalf("expected failure summary mentioning worker-1, got %q", stderr.String())
	}
}

func TestRunMultiExecTimeout(t *testing.T) {
	slowClient := &slowExecClient{delay: 5 * time.Second}
	pods := []kube.PodSummary{
		{Name: "pod-0", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), slowClient, "default", pods, "dev", []string{"sh", "-c", "sleep 100"}, "", 50*time.Millisecond, false, &stdout, &stderr)
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
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-c", "echo hello"}, "", 0, true, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got != "hello\n" {
		t.Fatalf("expected raw output, got %q", got)
	}
}
