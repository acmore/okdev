package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	spec := detachJobSpec{
		JobID:     "job-123",
		Pod:       "okdev-sess-worker-0",
		Container: "dev",
		Command:   "python train.py --epochs 100",
		StartedAt: "2026-04-23T03:00:00Z",
		LogPath:   "/tmp/okdev-exec/job-123.log",
		MetaPath:  "/tmp/okdev-exec/job-123.json",
	}
	got := detachCommand(spec)
	if len(got) != 3 {
		t.Fatalf("expected shell command triplet, got %v", got)
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("unexpected launcher prefix: %v", got)
	}
	script := got[2]
	for _, want := range []string{
		"mkdir -p '/tmp/okdev-exec'",
		"nohup sh -c",
		"python train.py --epochs 100",
		"meta_path='/tmp/okdev-exec/job-123.json'",
		"rc=$?",
		"state\":\"running\"",
		"state\":\"exited\"",
		"printf '%s\\n' \"$launch_json\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected detach script to contain %q, got %q", want, script)
		}
	}
}

func TestDetachCommandWithQuotes(t *testing.T) {
	spec := detachJobSpec{
		JobID:     "job-quoted",
		Pod:       "worker-0",
		Container: "dev",
		Command:   "echo 'hello world'",
		StartedAt: "2026-04-23T03:00:00Z",
		LogPath:   "/tmp/okdev-exec/job-quoted.log",
		MetaPath:  "/tmp/okdev-exec/job-quoted.json",
	}
	got := detachCommand(spec)
	if len(got) != 3 {
		t.Fatalf("expected shell command triplet, got %v", got)
	}
	if !strings.Contains(got[2], "nohup sh -c") || !strings.Contains(got[2], "echo '\\''hello world'\\''") {
		t.Fatalf("expected quoted user command to survive detach wrapper, got %q", got[2])
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
	launch, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-worker-0",
		PID:      48217,
		LogPath:  "/tmp/okdev-exec/job-worker-0.log",
		MetaPath: "/tmp/okdev-exec/job-worker-0.json",
	})
	launch2, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-worker-1",
		PID:      49102,
		LogPath:  "/tmp/okdev-exec/job-worker-1.log",
		MetaPath: "/tmp/okdev-exec/job-worker-1.json",
	})
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": string(launch) + "\n",
			"okdev-sess-worker-1": string(launch2) + "\n",
		},
		errs: map[string]error{},
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
	got := stdout.String()
	for _, want := range []string{
		"worker-0] detached job_id=job-worker-0 pid=48217 log=/tmp/okdev-exec/job-worker-0.log meta=/tmp/okdev-exec/job-worker-0.json",
		"worker-1] detached job_id=job-worker-1 pid=49102 log=/tmp/okdev-exec/job-worker-1.log meta=/tmp/okdev-exec/job-worker-1.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected detach metadata output %q, got %q", want, got)
		}
	}
}

func TestRunDetachExecWithError(t *testing.T) {
	launch, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-worker-0",
		PID:      48217,
		LogPath:  "/tmp/okdev-exec/job-worker-0.log",
		MetaPath: "/tmp/okdev-exec/job-worker-0.json",
	})
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": string(launch) + "\n",
		},
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
	if !strings.Contains(stdout.String(), "worker-0] detached job_id=job-worker-0 pid=48217") {
		t.Fatalf("expected detach for worker-0, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "worker-1] error:") {
		t.Fatalf("expected error for worker-1, got %q", stdout.String())
	}
}

func TestRunDetachExecRequiresLaunchMetadata(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "detached\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", "python train.py", pdshDefaultFanout, &stdout)
	if err == nil {
		t.Fatal("expected metadata parse error")
	}
	if !strings.Contains(stdout.String(), "worker-0] error: missing detach launch metadata") {
		t.Fatalf("expected metadata parse error to be surfaced, got %q", stdout.String())
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
