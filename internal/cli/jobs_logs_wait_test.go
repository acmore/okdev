package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestNewJobsLogsCmdAddsFollowAlias(t *testing.T) {
	cmd := newJobsLogsCmd(&Options{})
	flag := cmd.Flags().Lookup("follow")
	if flag == nil {
		t.Fatal("expected --follow flag")
	}
	if flag.Shorthand != "f" {
		t.Fatalf("expected -f shorthand, got %q", flag.Shorthand)
	}
}

func TestRunJobsLogsFollowStreamsPrefixedPodLogsUntilTerminal(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "running", nil),
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(0)),
			},
			"okdev-sess-worker-1": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-1", "dev", 101, "running", nil),
				detachMetadataJSON("job-shared", "okdev-sess-worker-1", "dev", 101, "exited", intPtr(0)),
			},
		},
		streamPlans: map[string]fakeJobsStreamPlan{
			"okdev-sess-worker-0|follow": {stdout: "alpha\n", waitForCancel: true},
			"okdev-sess-worker-1|follow": {stdout: "beta\n", waitForCancel: true},
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var out bytes.Buffer
	if err := runJobsLogs(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, true, &out); err != nil {
		t.Fatalf("runJobsLogs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"[worker-0] alpha", "[worker-1] beta"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
	if strings.Contains(got, "FAILED:") {
		t.Fatalf("expected no failure footer, got %q", got)
	}
}

func TestRunJobsWaitBlocksUntilAllPodsExitSuccessfully(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "running", nil),
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(0)),
			},
			"okdev-sess-worker-1": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-1", "dev", 101, "running", nil),
				detachMetadataJSON("job-shared", "okdev-sess-worker-1", "dev", 101, "exited", intPtr(0)),
			},
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	if err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout); err != nil {
		t.Fatalf("runJobsWait: %v", err)
	}
}

func TestRunJobsWaitFailsOnNonZeroExit(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(137)),
			},
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
	}
	err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "failed(1/1)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

type fakeJobsClient struct {
	mu              sync.Mutex
	listOutputs     map[string][]string
	listErrs        map[string][]error
	listCalls       map[string]int
	streamPlans     map[string]fakeJobsStreamPlan
	execShResponses map[string]fakeJobsExecResponse
	execScripts     []string
	onExec         func(pod, key string)
}

type fakeJobsStreamPlan struct {
	stdout        string
	stderr        string
	err           error
	waitForCancel bool
}

type fakeJobsExecResponse struct {
	stdout string
	err    error
}

func (f *fakeJobsClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return f.ExecInteractiveInContainer(ctx, namespace, pod, "", tty, command, stdin, stdout, stderr)
}

func (f *fakeJobsClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	f.mu.Lock()
	if f.listCalls == nil {
		f.listCalls = make(map[string]int)
	}
	idx := f.listCalls[pod]
	f.listCalls[pod]++
	outputs := f.listOutputs[pod]
	errs := f.listErrs[pod]
	var out string
	var err error
	if len(outputs) > 0 {
		if idx >= len(outputs) {
			out = outputs[len(outputs)-1]
		} else {
			out = outputs[idx]
		}
	}
	if len(errs) > 0 {
		if idx >= len(errs) {
			err = errs[len(errs)-1]
		} else {
			err = errs[idx]
		}
	}
	f.mu.Unlock()
	if out != "" {
		fmt.Fprint(stdout, out)
	}
	return err
}

func (f *fakeJobsClient) ExecShInContainer(ctx context.Context, namespace, pod, container, script string) ([]byte, error) {
	f.mu.Lock()
	f.execScripts = append(f.execScripts, pod+":"+script)
	key := execKeyForScript(script)
	resp := f.execShResponses[pod+"|"+key]
	onExec := f.onExec
	f.mu.Unlock()
	if onExec != nil {
		onExec(pod, key)
	}
	return []byte(resp.stdout), resp.err
}

func (f *fakeJobsClient) StreamShInContainer(ctx context.Context, namespace, pod, container, script string, stdout, stderr io.Writer) error {
	f.mu.Lock()
	key := "read"
	if strings.Contains(script, "tail -n +1 -f") {
		key = "follow"
	}
	plan, ok := f.streamPlans[pod+"|"+key]
	f.mu.Unlock()
	if !ok {
		return fmt.Errorf("unexpected stream script for %s: %s", pod, script)
	}
	if plan.stdout != "" {
		fmt.Fprint(stdout, plan.stdout)
	}
	if plan.stderr != "" {
		fmt.Fprint(stderr, plan.stderr)
	}
	if plan.waitForCancel {
		<-ctx.Done()
		return ctx.Err()
	}
	return plan.err
}

func execKeyForScript(script string) string {
	switch {
	case strings.Contains(script, "kill -TERM"):
		return "TERM"
	case strings.Contains(script, "kill -KILL"):
		return "KILL"
	default:
		return "other"
	}
}

func detachMetadataJSON(jobID, pod, container string, pid int, state string, exitCode *int) string {
	exitPart := ""
	if exitCode != nil {
		exitPart = fmt.Sprintf(",\"exitCode\":%d", *exitCode)
	}
	return fmt.Sprintf("{\"jobId\":\"%s\",\"pod\":\"%s\",\"container\":\"%s\",\"pid\":%d,\"command\":\"python train.py\",\"startedAt\":\"2026-05-09T10:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/%s.log\",\"stderrPath\":\"/tmp/okdev-exec/%s.log\",\"metaPath\":\"/tmp/okdev-exec/%s.json\",\"state\":\"%s\"%s}\n",
		jobID, pod, container, pid, jobID, jobID, jobID, state, exitPart)
}
