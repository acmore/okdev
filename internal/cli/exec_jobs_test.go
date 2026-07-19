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
		// Per-line output is "<alive>\t<groupLive>\t<json>".
		"printf '%s\\t%s\\t%s\\n' \"$alive\" \"$glive\" \"$contents\"",
		// Group liveness: count non-zombie members of the job's process
		// group, identity-checked via the inherited OKDEV_JOB_ID env var.
		`"pgid":\([0-9][0-9]*\)`,
		"/proc/[0-9]*/stat",
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
	mu          sync.Mutex
	calls       []string
	outputs     map[string]string
	errs        map[string]error
	restartInfo map[string]*kube.ContainerRestartInfo
}

func (f *fakeContainerExecClient) GetContainerRestartInfo(ctx context.Context, namespace, pod, container string) (*kube.ContainerRestartInfo, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.restartInfo[pod+"|"+container], nil
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

func TestCollectDetachJobsErrorIncludesTerminationHint(t *testing.T) {
	// When both the target container and the sidecar fallback fail, the pod
	// error should say WHY the container is gone instead of a bare
	// "container not found".
	client := &fakeContainerExecClient{
		errs: map[string]error{
			"okdev-sess-worker-0|pytorch":                   errors.New(`container not found ("pytorch")`),
			"okdev-sess-worker-0|" + syncthingContainerName: errors.New("sidecar unavailable"),
		},
		restartInfo: map[string]*kube.ContainerRestartInfo{
			"okdev-sess-worker-0|pytorch": {
				LastReason:     "OOMKilled",
				LastExitCode:   137,
				LastFinishedAt: time.Date(2026, 7, 5, 6, 32, 11, 0, time.UTC),
			},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	_, podErrors := collectDetachJobs(context.Background(), client, "default", pods, "pytorch", "", pdshDefaultFanout)
	if len(podErrors) != 1 {
		t.Fatalf("expected one pod error, got %+v", podErrors)
	}
	if !strings.Contains(podErrors[0].Error, `container "pytorch" terminated: OOMKilled (exit 137, finished 2026-07-05T06:32:11Z)`) {
		t.Fatalf("expected termination hint in error, got %q", podErrors[0].Error)
	}
}

func TestContainerUnavailableHint(t *testing.T) {
	client := &fakeContainerExecClient{
		restartInfo: map[string]*kube.ContainerRestartInfo{
			"pod-1|pytorch": {LastReason: "OOMKilled", LastExitCode: 137},
		},
	}
	got := containerUnavailableHint(context.Background(), client, "ns", "pod-1", "pytorch")
	if got != `container "pytorch" terminated: OOMKilled (exit 137)` {
		t.Fatalf("unexpected hint: %q", got)
	}
	if hint := containerUnavailableHint(context.Background(), client, "ns", "pod-1", "other"); hint != "" {
		t.Fatalf("expected empty hint without termination info, got %q", hint)
	}
	// Clients without restart-info capability (plain ExecClient) yield "".
	if hint := containerUnavailableHint(context.Background(), &fakePdshExecClient{}, "ns", "pod-1", "pytorch"); hint != "" {
		t.Fatalf("expected empty hint for capability-less client, got %q", hint)
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

func TestParseDetachMetadataLinesParsesGroupLive(t *testing.T) {
	raw := "1\t3\t{\"jobId\":\"job-a\",\"pod\":\"p\",\"container\":\"dev\",\"pid\":100,\"pgid\":100,\"metaPath\":\"/var/okdev/exec/job-a.json\",\"state\":\"running\"}\n" +
		// Older two-field lines and raw JSON must keep parsing (mixed cli
		// versions against the same pod).
		"0\t{\"jobId\":\"job-b\",\"pod\":\"p\",\"container\":\"dev\",\"pid\":101,\"metaPath\":\"/tmp/okdev-exec/job-b.json\",\"state\":\"running\"}\n"
	jobs, err := parseDetachMetadataLines(raw)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(jobs) != 2 {
		t.Fatalf("expected 2 jobs, got %+v", jobs)
	}
	if jobs[0].GroupLive != 3 || jobs[0].PGID != 100 {
		t.Fatalf("expected groupLive=3 pgid=100, got %+v", jobs[0])
	}
	if jobs[1].GroupLive != 0 || jobs[1].State != "orphaned" {
		t.Fatalf("expected legacy line orphaned with groupLive=0, got %+v", jobs[1])
	}
}

func TestJobsListCommandLimitForWidth(t *testing.T) {
	jobs := []logicalExecJobView{{
		JobID:        "20260705T063211Z-deadbeef", // 25 chars
		SummaryState: "running(4/4)",              // 12 chars
		Pods:         4,
		StartedAt:    "2026-07-05T06:32:11Z", // 20 chars
	}}
	// fixed = 25 + 12 + len("PODS")=4 + 20 + 4*2 = 69
	if got := jobsListCommandLimitForWidth(120, jobs); got != 51 {
		t.Fatalf("expected limit 51, got %d", got)
	}
	// Narrow terminals keep a recognizable floor instead of a useless stub.
	if got := jobsListCommandLimitForWidth(60, jobs); got != 16 {
		t.Fatalf("expected floor 16, got %d", got)
	}
	// Unknown width disables truncation.
	if got := jobsListCommandLimitForWidth(0, jobs); got != 0 {
		t.Fatalf("expected no limit, got %d", got)
	}
}

func TestFlattenCommandForTable(t *testing.T) {
	// Real-world detach commands are multi-line `bash -c` strings with
	// backslash continuations; the table must render them as one row.
	in := "bash -c 'cd /opt/train/recipe/pretrain/script && \\\nPYTORCH_CUDA_ALLOC_CONF=expandable_segments:True \\\nMODEL_PATH=/data/models/base-model \\\n\tpython pretrain.py'"
	got := flattenCommandForTable(in)
	if strings.ContainsAny(got, "\n\r\t") {
		t.Fatalf("expected flattened single-line command, got %q", got)
	}
	want := "bash -c 'cd /opt/train/recipe/pretrain/script && PYTORCH_CUDA_ALLOC_CONF=expandable_segments:True MODEL_PATH=/data/models/base-model python pretrain.py'"
	if got != want {
		t.Fatalf("unexpected flatten result:\n got %q\nwant %q", got, want)
	}
	// Newlines without continuation backslashes also collapse.
	if got := flattenCommandForTable("echo a\necho b"); got != "echo a echo b" {
		t.Fatalf("unexpected: %q", got)
	}
	if got := flattenCommandForTable("plain command"); got != "plain command" {
		t.Fatalf("single-line commands must pass through, got %q", got)
	}
}

func TestRunExecJobsTableRendersMultilineCommandAsOneRow(t *testing.T) {
	meta := "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":100," +
		"\"command\":\"bash -c 'cd /x && \\\\\\nENV=1 \\\\\\npython t.py'\"," +
		"\"startedAt\":\"2026-07-05T00:06:22Z\",\"stdoutPath\":\"/var/okdev/exec/job-a.log\",\"stderrPath\":\"/var/okdev/exec/job-a.log\",\"metaPath\":\"/var/okdev/exec/job-a.json\",\"state\":\"running\"}\n"
	client := &fakePdshExecClient{outputs: map[string]string{"okdev-sess-worker-0": meta}}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	if err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false, fanoutRoute{}); err != nil {
		t.Fatalf("runExecJobs: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "bash -c 'cd /x && ENV=1 python t.py'") {
		t.Fatalf("expected flattened command in table, got %q", got)
	}
	for _, line := range strings.Split(got, "\n") {
		if strings.Contains(line, "hint:") {
			continue
		}
		if strings.Contains(line, "job-a") && !strings.Contains(line, "python t.py'") {
			t.Fatalf("expected the job row to stay on one line, got %q", got)
		}
	}
	// A running job points at jobs wait (#191).
	if !strings.Contains(got, "okdev jobs wait job-a") {
		t.Fatalf("expected wait hint for running job, got %q", got)
	}
}

func TestTruncateTableCell(t *testing.T) {
	if got := truncateTableCell("short", 10); got != "short" {
		t.Fatalf("unexpected: %q", got)
	}
	if got := truncateTableCell("torchrun --nnodes 4 --nproc-per-node 8 pretrain.py", 20); got != "torchrun --nnodes 4…" {
		t.Fatalf("unexpected truncation: %q", got)
	}
	if got := truncateTableCell("anything", 0); got != "anything" {
		t.Fatalf("limit 0 must disable truncation: %q", got)
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
