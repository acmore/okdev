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
	"sync/atomic"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

// trackingExecClient records per-pod execution timing to verify fanout behavior.
type trackingExecClient struct {
	mu       sync.Mutex
	delay    time.Duration
	started  map[string]time.Time
	finished map[string]time.Time
	outputs  map[string]string
	errs     map[string]error
}

func newTrackingExecClient(delay time.Duration) *trackingExecClient {
	return &trackingExecClient{
		delay:    delay,
		started:  make(map[string]time.Time),
		finished: make(map[string]time.Time),
		outputs:  make(map[string]string),
		errs:     make(map[string]error),
	}
}

func seedDetachLaunchOutputs(client *trackingExecClient, pods []kube.PodSummary) {
	for i, pod := range pods {
		payload, _ := json.Marshal(detachLaunchInfo{
			JobID:    fmt.Sprintf("job-%d", i),
			PID:      40000 + i,
			LogPath:  fmt.Sprintf("/tmp/okdev-exec/job-%d.log", i),
			MetaPath: fmt.Sprintf("/tmp/okdev-exec/job-%d.json", i),
		})
		client.outputs[pod.Name] = string(payload) + "\n"
	}
}

func (t *trackingExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	t.mu.Lock()
	t.started[pod] = time.Now()
	output := t.outputs[pod]
	podErr := t.errs[pod]
	t.mu.Unlock()

	if output != "" {
		fmt.Fprint(stdout, output)
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(t.delay):
	}

	t.mu.Lock()
	t.finished[pod] = time.Now()
	t.mu.Unlock()
	return podErr
}

func (t *trackingExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return t.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

// maxConcurrent returns the peak number of pods executing simultaneously.
func (t *trackingExecClient) maxConcurrent() int {
	t.mu.Lock()
	defer t.mu.Unlock()

	type event struct {
		t     time.Time
		delta int
	}
	events := make([]event, 0, len(t.started)+len(t.finished))
	for pod, ts := range t.started {
		events = append(events, event{t: ts, delta: 1})
		if tf, ok := t.finished[pod]; ok {
			events = append(events, event{t: tf, delta: -1})
		}
	}
	// sort by time
	for i := range events {
		for j := i + 1; j < len(events); j++ {
			if events[j].t.Before(events[i].t) {
				events[i], events[j] = events[j], events[i]
			}
		}
	}
	var cur, peak int
	for _, e := range events {
		cur += e.delta
		if cur > peak {
			peak = cur
		}
	}
	return peak
}

// --- Fanout E2E Tests ---

func TestRunMultiExecFanoutLimitsParallelism(t *testing.T) {
	// 8 pods with fanout=2 — at most 2 should execute concurrently.
	pods := make([]kube.PodSummary, 8)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("okdev-sess-worker-%d", i), Phase: "Running"}
	}

	client := newTrackingExecClient(50 * time.Millisecond)
	for _, p := range pods {
		client.outputs[p.Name] = "ok\n"
	}

	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev",
		[]string{"sh", "-c", "echo ok"}, "", 0, false, 2, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	peak := client.maxConcurrent()
	if peak > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", peak)
	}
	// All 8 pods should have been executed.
	if len(client.started) != 8 {
		t.Fatalf("expected 8 pods executed, got %d", len(client.started))
	}
}

func TestRunMultiExecFanoutOneIsSerial(t *testing.T) {
	pods := make([]kube.PodSummary, 4)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("pod-%d", i), Phase: "Running"}
	}

	client := newTrackingExecClient(20 * time.Millisecond)
	for _, p := range pods {
		client.outputs[p.Name] = "ok\n"
	}

	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev",
		[]string{"sh", "-c", "echo ok"}, "", 0, false, 1, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	peak := client.maxConcurrent()
	if peak > 1 {
		t.Fatalf("expected serial execution (max concurrency 1), got %d", peak)
	}
}

func TestRunDetachExecFanoutLimitsParallelism(t *testing.T) {
	pods := make([]kube.PodSummary, 6)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("okdev-sess-worker-%d", i), Phase: "Running"}
	}

	client := newTrackingExecClient(30 * time.Millisecond)
	seedDetachLaunchOutputs(client, pods)

	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev",
		"python train.py", 2, &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	peak := client.maxConcurrent()
	if peak > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", peak)
	}
	if len(client.started) != 6 {
		t.Fatalf("expected 6 pods detached, got %d", len(client.started))
	}
	// All pods should show detached in output.
	for i := 0; i < 6; i++ {
		if !strings.Contains(stdout.String(), "detached") {
			t.Fatalf("expected detach confirmation for all pods, got %q", stdout.String())
		}
	}
}

// --- Full pipeline E2E: filter → execute → output ---

func TestMultiExecPipelineAllPodsWithPrefixedOutput(t *testing.T) {
	// Simulate the full pipeline: filter running pods, execute, verify prefixed output.
	allPods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-sess-worker-1", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-sess-master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "okdev-sess-pending-0", Phase: "Pending", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}

	// Step 1: filter running
	running := filterRunningPods(allPods)
	if len(running) != 3 {
		t.Fatalf("expected 3 running pods, got %d", len(running))
	}

	// Step 2: filter by role
	workers := filterPodsByRole(running, "worker")
	if len(workers) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(workers))
	}

	// Step 3: execute
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "gpu0: NVIDIA A100\n",
			"okdev-sess-worker-1": "gpu1: NVIDIA H100\n",
		},
		errs: map[string]error{},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", workers, "dev",
		[]string{"sh", "-lc", "nvidia-smi"}, "", 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Step 4: verify prefixed output
	output := stdout.String()
	if !strings.Contains(output, "[worker-0] gpu0: NVIDIA A100") {
		t.Fatalf("expected prefixed output for worker-0, got %q", output)
	}
	if !strings.Contains(output, "[worker-1] gpu1: NVIDIA H100") {
		t.Fatalf("expected prefixed output for worker-1, got %q", output)
	}
}

func TestMultiExecPipelineExcludeAndLogDir(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
		{Name: "okdev-sess-worker-2", Phase: "Running"},
	}

	// Exclude worker-1
	filtered := excludePods(pods, []string{"okdev-sess-worker-1"})
	if len(filtered) != 2 {
		t.Fatalf("expected 2 pods after exclude, got %d", len(filtered))
	}

	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "line-a\n",
			"okdev-sess-worker-2": "line-b\n",
		},
		errs: map[string]error{},
	}

	logDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", filtered, "dev",
		[]string{"sh", "-c", "echo test"}, logDir, 0, false, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify log files were created for the right pods (not worker-1).
	for _, name := range []string{"worker-0.log", "worker-2.log"} {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
	// worker-1 log should NOT exist.
	if _, err := os.Stat(filepath.Join(logDir, "worker-1.log")); err == nil {
		t.Fatal("worker-1.log should not exist after exclusion")
	}
}

func TestMultiExecPipelineLabelFilterWithTimeout(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "pod-a", Phase: "Running", Labels: map[string]string{"gpu": "a100", "zone": "us-east"}},
		{Name: "pod-b", Phase: "Running", Labels: map[string]string{"gpu": "a100", "zone": "eu-west"}},
		{Name: "pod-c", Phase: "Running", Labels: map[string]string{"gpu": "h100", "zone": "us-east"}},
	}

	// Filter by gpu=a100 AND zone=us-east
	filtered := filterPodsByLabels(pods, []string{"gpu=a100", "zone=us-east"})
	if len(filtered) != 1 || filtered[0].Name != "pod-a" {
		t.Fatalf("expected only pod-a, got %v", filtered)
	}

	// Use slow client with a tight timeout to verify timeout behavior.
	slowClient := &slowExecClient{delay: 5 * time.Second}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), slowClient, "default", filtered, "dev",
		[]string{"sh", "-c", "sleep 100"}, "", 50*time.Millisecond, false, pdshDefaultFanout, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !strings.Contains(err.Error(), "1 of 1 pods failed") {
		t.Fatalf("unexpected error message: %v", err)
	}
}

func TestMultiExecPipelinePartialFailureWithDetach(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
		{Name: "okdev-sess-worker-2", Phase: "Running"},
	}

	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-0\",\"pid\":40000,\"logPath\":\"/tmp/okdev-exec/job-0.log\",\"metaPath\":\"/tmp/okdev-exec/job-0.json\"}\n",
			"okdev-sess-worker-2": "{\"jobId\":\"job-2\",\"pid\":40002,\"logPath\":\"/tmp/okdev-exec/job-2.log\",\"metaPath\":\"/tmp/okdev-exec/job-2.json\"}\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("container not ready"),
		},
	}

	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev",
		"python train.py", pdshDefaultFanout, &stdout)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if !strings.Contains(err.Error(), "1 of 3") {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stdout.String()
	if !strings.Contains(output, "worker-0] detached") {
		t.Fatalf("expected worker-0 detached, got %q", output)
	}
	if !strings.Contains(output, "worker-1] error: container not ready") {
		t.Fatalf("expected worker-1 error, got %q", output)
	}
	if !strings.Contains(output, "worker-2] detached") {
		t.Fatalf("expected worker-2 detached, got %q", output)
	}
}

func TestMultiExecPipelineNoPrefixModeMultiplePods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}

	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "alpha\n",
			"okdev-sess-worker-1": "bravo\n",
		},
		errs: map[string]error{},
	}

	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev",
		[]string{"sh", "-c", "echo test"}, "", 0, true, pdshDefaultFanout, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	output := stdout.String()
	// In no-prefix mode, output should NOT contain the pod name prefix.
	if strings.Contains(output, "[worker-0]") || strings.Contains(output, "[worker-1]") {
		t.Fatalf("expected no prefix in output, got %q", output)
	}
	if !strings.Contains(output, "alpha") || !strings.Contains(output, "bravo") {
		t.Fatalf("expected raw output from both pods, got %q", output)
	}
}

// --- Cancellation E2E ---

func TestRunMultiExecCancelledContext(t *testing.T) {
	pods := make([]kube.PodSummary, 4)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("pod-%d", i), Phase: "Running"}
	}

	client := newTrackingExecClient(5 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after a short delay to interrupt in-flight executions.
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	var stdout, stderr bytes.Buffer
	err := runMultiExec(ctx, client, "default", pods, "dev",
		[]string{"sh", "-c", "sleep 100"}, "", 0, false, 2, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

func TestRunDetachExecCancelledContext(t *testing.T) {
	pods := make([]kube.PodSummary, 4)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("pod-%d", i), Phase: "Running"}
	}

	client := newTrackingExecClient(5 * time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()

	var stdout bytes.Buffer
	err := runDetachExec(ctx, client, "default", pods, "dev",
		"python train.py", 2, &stdout)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
}

// --- Large pod count E2E ---

func TestRunMultiExecLargePodSetWithFanout(t *testing.T) {
	const numPods = 50
	pods := make([]kube.PodSummary, numPods)
	for i := range pods {
		pods[i] = kube.PodSummary{Name: fmt.Sprintf("okdev-sess-worker-%d", i), Phase: "Running"}
	}

	var execCount atomic.Int32
	client := &countingExecClient{
		count:  &execCount,
		output: "done\n",
	}

	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev",
		[]string{"sh", "-c", "echo done"}, "", 0, false, 8, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if int(execCount.Load()) != numPods {
		t.Fatalf("expected %d executions, got %d", numPods, execCount.Load())
	}
}

// countingExecClient counts total executions atomically.
type countingExecClient struct {
	count  *atomic.Int32
	output string
}

func (c *countingExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	c.count.Add(1)
	if c.output != "" {
		fmt.Fprint(stdout, c.output)
	}
	return nil
}

func (c *countingExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return c.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

// --- Combined filter + fanout E2E ---

func TestMultiExecFullPipelineRoleFilterFanoutLogDir(t *testing.T) {
	// End-to-end: start with a mixed pod set, filter by role, apply exclude,
	// execute with bounded fanout, write logs.
	allPods := []kube.PodSummary{
		{Name: "okdev-train-worker-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-train-worker-1", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-train-worker-2", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-train-worker-3", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-train-master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "okdev-train-ps-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "PS"}},
		{Name: "okdev-train-pending-0", Phase: "Pending", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}

	// Pipeline: running → role=worker → exclude worker-2
	running := filterRunningPods(allPods)
	workers := filterPodsByRole(running, "worker")
	filtered := excludePods(workers, []string{"okdev-train-worker-2"})

	if len(filtered) != 3 {
		t.Fatalf("expected 3 pods after pipeline, got %d", len(filtered))
	}

	client := newTrackingExecClient(20 * time.Millisecond)
	for _, p := range filtered {
		client.outputs[p.Name] = fmt.Sprintf("output from %s\n", p.Name)
	}

	logDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", filtered, "dev",
		[]string{"sh", "-c", "echo test"}, logDir, 0, false, 2, &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Verify fanout was respected.
	peak := client.maxConcurrent()
	if peak > 2 {
		t.Fatalf("expected max concurrency <= 2, got %d", peak)
	}

	// Verify all 3 pods executed.
	if len(client.started) != 3 {
		t.Fatalf("expected 3 pods executed, got %d", len(client.started))
	}

	// Verify log files.
	expectedLogs := []string{"worker-0.log", "worker-1.log", "worker-3.log"}
	for _, name := range expectedLogs {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
	// Excluded pod should not have a log file.
	if _, err := os.Stat(filepath.Join(logDir, "worker-2.log")); err == nil {
		t.Fatal("worker-2.log should not exist after exclusion")
	}

	// Verify prefixed output present.
	output := stdout.String()
	if !strings.Contains(output, "[worker-0]") || !strings.Contains(output, "[worker-1]") || !strings.Contains(output, "[worker-3]") {
		t.Fatalf("expected prefixed output for workers 0,1,3, got %q", output)
	}
}
