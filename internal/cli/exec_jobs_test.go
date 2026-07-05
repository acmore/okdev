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

	"github.com/acmore/okdev/internal/kube"
)

func TestCollectDetachJobs(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48217,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-a.log\",\"stderrPath\":\"/tmp/okdev-exec/job-a.log\",\"metaPath\":\"/tmp/okdev-exec/job-a.json\",\"state\":\"running\"}\n",
			"okdev-sess-worker-1": "{\"jobId\":\"job-b\",\"pod\":\"okdev-sess-worker-1\",\"container\":\"dev\",\"pid\":49102,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:01:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-b.log\",\"stderrPath\":\"/tmp/okdev-exec/job-b.log\",\"metaPath\":\"/tmp/okdev-exec/job-b.json\",\"state\":\"exited\",\"exitCode\":137}\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	rows, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout)
	if len(podErrors) != 0 {
		t.Fatalf("unexpected pod errors: %+v", podErrors)
	}
	if len(rows) != 2 {
		t.Fatalf("expected 2 jobs, got %d", len(rows))
	}
	if rows[0].Pod != "okdev-sess-worker-0" || rows[0].State != "running" || rows[0].ExitCode != nil {
		t.Fatalf("unexpected first row %+v", rows[0])
	}
	if rows[1].Pod != "okdev-sess-worker-1" || rows[1].State != "exited" || rows[1].ExitCode == nil || *rows[1].ExitCode != 137 {
		t.Fatalf("unexpected second row %+v", rows[1])
	}
}

func TestCollectDetachJobsAggregatesPerPodErrors(t *testing.T) {
	// A flaky pod must not hide the healthy pod's jobs from the listing.
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":100,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-a.log\",\"stderrPath\":\"/tmp/okdev-exec/job-a.log\",\"metaPath\":\"/tmp/okdev-exec/job-a.json\",\"state\":\"running\"}\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("container not ready"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	rows, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout)
	if len(rows) != 1 || rows[0].Pod != "okdev-sess-worker-0" {
		t.Fatalf("expected healthy pod's job to be retained, got %+v", rows)
	}
	if len(podErrors) != 1 || podErrors[0].Pod != "okdev-sess-worker-1" {
		t.Fatalf("expected failure recorded for worker-1, got %+v", podErrors)
	}
	if !strings.Contains(podErrors[0].Error, "container not ready") {
		t.Fatalf("unexpected error message: %q", podErrors[0].Error)
	}
}

func TestRunExecJobsRendersPartialFailureFooter(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":100,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-a.log\",\"stderrPath\":\"/tmp/okdev-exec/job-a.log\",\"metaPath\":\"/tmp/okdev-exec/job-a.json\",\"state\":\"running\"}\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("container not ready"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var out bytes.Buffer
	err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false, fanoutRoute{})
	if err == nil {
		t.Fatal("expected error reflecting partial failure")
	}
	if !strings.Contains(err.Error(), "1 of 2 pods failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	// Healthy logical job row must still render.
	for _, want := range []string{"JOB ID", "job-a", "running(1/1)", "python train.py"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected table to include healthy pod row, missing %q in %q", want, got)
		}
	}
	// Failure footer must surface the failing pod and reason.
	if !strings.Contains(got, "FAILED:") || !strings.Contains(got, "pod=okdev-sess-worker-1") || !strings.Contains(got, "container not ready") {
		t.Fatalf("expected FAILED footer with worker-1 details, got %q", got)
	}
}

func TestRunExecJobs(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-shared\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48217,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-shared.log\",\"stderrPath\":\"/tmp/okdev-exec/job-shared.log\",\"metaPath\":\"/tmp/okdev-exec/job-shared.json\",\"state\":\"running\"}\n",
			"okdev-sess-worker-1": "{\"jobId\":\"job-shared\",\"pod\":\"okdev-sess-worker-1\",\"container\":\"dev\",\"pid\":49102,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:01Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-shared.log\",\"stderrPath\":\"/tmp/okdev-exec/job-shared.log\",\"metaPath\":\"/tmp/okdev-exec/job-shared.json\",\"state\":\"exited\",\"exitCode\":0}\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var out bytes.Buffer
	if err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false, fanoutRoute{}); err != nil {
		t.Fatalf("runExecJobs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"JOB ID", "STATE", "PODS", "COMMAND", "job-shared", "running(1/2)", "2", "python train.py"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected table output containing %q, got %q", want, got)
		}
	}
}

func TestRunExecJobsJSONGroupsLogicalJobs(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-shared\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48217,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-shared.log\",\"stderrPath\":\"/tmp/okdev-exec/job-shared.log\",\"metaPath\":\"/tmp/okdev-exec/job-shared.json\",\"state\":\"running\"}\n",
			"okdev-sess-worker-1": "{\"jobId\":\"job-shared\",\"pod\":\"okdev-sess-worker-1\",\"container\":\"dev\",\"pid\":49102,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:01Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-shared.log\",\"stderrPath\":\"/tmp/okdev-exec/job-shared.log\",\"metaPath\":\"/tmp/okdev-exec/job-shared.json\",\"state\":\"exited\",\"exitCode\":0}\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var out bytes.Buffer
	if err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, true, fanoutRoute{}); err != nil {
		t.Fatalf("runExecJobs: %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"\"jobId\": \"job-shared\"",
		"\"state\": \"running(1/2)\"",
		"\"pods\": 2",
		"\"podStates\": [",
		"\"pod\": \"okdev-sess-worker-0\"",
		"\"pod\": \"okdev-sess-worker-1\"",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected grouped json output containing %q, got %q", want, got)
		}
	}
}

func TestParseDetachMetadataLinesDowngradesRunningWhenPidDead(t *testing.T) {
	// The remote script tags each line with "<alive>\t<json>". When alive
	// is "0" and the metadata still claims "running", the listing should
	// surface that as "orphaned" so users do not chase a stale pid.
	raw := "0\t{\"jobId\":\"j1\",\"pod\":\"p1\",\"container\":\"dev\",\"pid\":42," +
		"\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\"," +
		"\"stdoutPath\":\"/tmp/okdev-exec/j1.log\",\"stderrPath\":\"/tmp/okdev-exec/j1.log\"," +
		"\"metaPath\":\"/tmp/okdev-exec/j1.json\",\"state\":\"running\"}\n"
	jobs, err := parseDetachMetadataLines(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(jobs) != 1 {
		t.Fatalf("expected 1 job, got %d", len(jobs))
	}
	if jobs[0].State != "orphaned" {
		t.Fatalf("expected orphaned, got %q", jobs[0].State)
	}
}

func TestParseDetachMetadataLinesAliveDoesNotChangeExited(t *testing.T) {
	// A completed job whose pid was reused (alive=0) should still report
	// the recorded exit state, not be downgraded to orphaned.
	raw := "0\t{\"jobId\":\"j1\",\"pod\":\"p1\",\"container\":\"dev\",\"pid\":42," +
		"\"command\":\"x\",\"startedAt\":\"2026-04-23T03:00:00Z\"," +
		"\"stdoutPath\":\"l\",\"stderrPath\":\"l\",\"metaPath\":\"m\"," +
		"\"state\":\"exited\",\"exitCode\":0}\n"
	jobs, err := parseDetachMetadataLines(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != "exited" {
		t.Fatalf("expected exited preserved, got %+v", jobs)
	}
}

func TestParseDetachMetadataLinesAcceptsRawJSONForBackwardsCompat(t *testing.T) {
	// Older pods may produce raw JSON without the alive prefix; treat them
	// as alive to avoid spurious orphan reports during a rollout.
	raw := "{\"jobId\":\"j1\",\"pod\":\"p1\",\"container\":\"dev\",\"pid\":42," +
		"\"command\":\"x\",\"startedAt\":\"2026-04-23T03:00:00Z\"," +
		"\"stdoutPath\":\"l\",\"stderrPath\":\"l\",\"metaPath\":\"m\",\"state\":\"running\"}\n"
	jobs, err := parseDetachMetadataLines(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(jobs) != 1 || jobs[0].State != "running" {
		t.Fatalf("expected running preserved for raw JSON line, got %+v", jobs)
	}
}

func TestDetachJobsCommandReconcilesAndReapsTmpFiles(t *testing.T) {
	cmd := detachJobsCommand()
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected detach jobs command shape: %v", cmd)
	}
	for _, want := range []string{
		// Liveness is established by matching OKDEV_JOB_ID in the pid's
		// /proc/<pid>/environ. Using an environment variable (rather than
		// cmdline) is robust against PID reuse even though the job's
		// cmdline is the arbitrary user command.
		"/proc/$pid/environ",
		"alive=1",
		"alive=0",
		"grep -Fqx \"OKDEV_JOB_ID=$job_id\"",
		// Per-line output is "<alive>\t<json>".
		"printf '%s\\t%s\\n' \"$alive\" \"$contents\"",
		// Best-effort cleanup of stale temp files left by killed wrappers.
		"-name '*.tmp'",
		"-mmin +1",
		// Jobs launched before the /var/okdev move must stay visible, so the
		// scan covers both the runtime-volume dir and the legacy /tmp dir.
		"'" + detachMetadataDir + "'",
		"'" + legacyDetachMetadataDir + "'",
	} {
		if !strings.Contains(cmd[2], want) {
			t.Fatalf("expected detach jobs command to contain %q, got %q", want, cmd[2])
		}
	}
}

// fakeContainerExecClient routes exec results by "pod|container" so tests can
// model a dead target container alongside a healthy sidecar.
type fakeContainerExecClient struct {
	mu      sync.Mutex
	calls   []string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakeContainerExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return f.ExecInteractiveInContainer(ctx, namespace, pod, "", tty, command, stdin, stdout, stderr)
}

func (f *fakeContainerExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	key := pod + "|" + container
	f.mu.Lock()
	f.calls = append(f.calls, key)
	output := f.outputs[key]
	err := f.errs[key]
	f.mu.Unlock()
	if output != "" {
		fmt.Fprint(stdout, output)
	}
	return err
}

func TestCollectDetachJobsFallsBackToSidecarWhenContainerGone(t *testing.T) {
	// An OOMKilled target container must not hide job metadata: it lives on
	// the shared /var/okdev volume, readable via the always-running sidecar.
	meta := "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"pytorch\",\"pid\":100,\"command\":\"python train.py\",\"startedAt\":\"2026-07-05T03:00:00Z\",\"stdoutPath\":\"/var/okdev/exec/job-a.log\",\"stderrPath\":\"/var/okdev/exec/job-a.log\",\"metaPath\":\"/var/okdev/exec/job-a.json\",\"state\":\"running\"}\n"
	client := &fakeContainerExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0|" + syncthingContainerName: meta,
		},
		errs: map[string]error{
			"okdev-sess-worker-0|pytorch": errors.New(`container not found ("pytorch")`),
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	rows, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "pytorch", "", pdshDefaultFanout)
	if len(podErrors) != 0 {
		t.Fatalf("unexpected pod errors: %+v", podErrors)
	}
	if len(rows) != 1 || rows[0].JobID != "job-a" {
		t.Fatalf("expected job-a via sidecar fallback, got %+v", rows)
	}
	// The row keeps the job's original container so stop/signal still target
	// the recorded runtime, not the sidecar.
	if rows[0].Container != "pytorch" {
		t.Fatalf("expected row container pytorch, got %q", rows[0].Container)
	}
	wantCalls := []string{"okdev-sess-worker-0|pytorch", "okdev-sess-worker-0|" + syncthingContainerName}
	if len(client.calls) != 2 || client.calls[0] != wantCalls[0] || client.calls[1] != wantCalls[1] {
		t.Fatalf("expected calls %v, got %v", wantCalls, client.calls)
	}
}

func TestCollectDetachJobsDoesNotFallBackOnCommandError(t *testing.T) {
	// Only container-unavailable errors justify the sidecar retry; a failing
	// list command must surface as a pod error without a second exec.
	client := &fakeContainerExecClient{
		errs: map[string]error{
			"okdev-sess-worker-0|pytorch": errors.New("command terminated with exit code 2"),
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	rows, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "pytorch", "", pdshDefaultFanout)
	if len(rows) != 0 {
		t.Fatalf("expected no rows, got %+v", rows)
	}
	if len(podErrors) != 1 || !strings.Contains(podErrors[0].Error, "exit code 2") {
		t.Fatalf("expected exit-code pod error, got %+v", podErrors)
	}
	if len(client.calls) != 1 {
		t.Fatalf("expected single exec attempt, got %v", client.calls)
	}
}

func TestIsContainerUnavailableExecError(t *testing.T) {
	cases := []struct {
		err  error
		want bool
	}{
		{nil, false},
		{errors.New(`container not found ("pytorch")`), true},
		{errors.New("container pytorch is not running"), true},
		{errors.New("container is not created or running"), true},
		{errors.New("cannot exec into a stopped container"), true},
		{errors.New("command terminated with exit code 1"), false},
		{errors.New("connection reset by peer"), false},
	}
	for _, tc := range cases {
		if got := isContainerUnavailableExecError(tc.err); got != tc.want {
			t.Fatalf("isContainerUnavailableExecError(%v) = %v, want %v", tc.err, got, tc.want)
		}
	}
}

func TestCollectDetachJobsFiltersByJobID(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48217,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-a.log\",\"stderrPath\":\"/tmp/okdev-exec/job-a.log\",\"metaPath\":\"/tmp/okdev-exec/job-a.json\",\"state\":\"running\"}\n{\"jobId\":\"job-b\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48218,\"command\":\"python eval.py\",\"startedAt\":\"2026-04-23T03:02:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-b.log\",\"stderrPath\":\"/tmp/okdev-exec/job-b.log\",\"metaPath\":\"/tmp/okdev-exec/job-b.json\",\"state\":\"exited\",\"exitCode\":0}\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
	}
	rows, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "dev", "job-b", pdshDefaultFanout)
	if len(podErrors) != 0 {
		t.Fatalf("unexpected pod errors: %+v", podErrors)
	}
	if len(rows) != 1 || rows[0].JobID != "job-b" {
		t.Fatalf("expected only job-b, got %+v", rows)
	}
}

func TestGroupDetachJobsSummarizesLogicalJob(t *testing.T) {
	rows := []execJobView{
		{
			Pod:       "okdev-sess-worker-0",
			Container: "dev",
			JobID:     "job-shared",
			PID:       100,
			State:     "running",
			LogPath:   "/tmp/okdev-exec/job-shared-worker-0.log",
			MetaPath:  "/tmp/okdev-exec/job-shared-worker-0.json",
			StartedAt: "2026-05-09T10:00:00Z",
			Command:   "python train.py",
		},
		{
			Pod:       "okdev-sess-worker-1",
			Container: "dev",
			JobID:     "job-shared",
			PID:       101,
			State:     "exited",
			ExitCode:  intPtr(0),
			LogPath:   "/tmp/okdev-exec/job-shared-worker-1.log",
			MetaPath:  "/tmp/okdev-exec/job-shared-worker-1.json",
			StartedAt: "2026-05-09T10:00:01Z",
			Command:   "python train.py",
		},
	}

	jobs := groupDetachJobs(rows)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 logical job, got %d", len(jobs))
	}
	if jobs[0].JobID != "job-shared" {
		t.Fatalf("unexpected job id %q", jobs[0].JobID)
	}
	if jobs[0].Pods != 2 {
		t.Fatalf("expected 2 pods, got %d", jobs[0].Pods)
	}
	if jobs[0].SummaryState != "running(1/2)" {
		t.Fatalf("unexpected summary state %q", jobs[0].SummaryState)
	}
	if jobs[0].Command != "python train.py" {
		t.Fatalf("unexpected command %q", jobs[0].Command)
	}
}

func intPtr(v int) *int { return &v }
