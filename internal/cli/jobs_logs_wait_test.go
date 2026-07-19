package cli

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

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
	if err := runJobsLogs(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, jobsLogsOptions{Follow: true, TailLines: -1}, &out); err != nil {
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
	if err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, jobsWaitOptions{}, io.Discard); err != nil {
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
	err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, jobsWaitOptions{}, io.Discard)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(err.Error(), "failed(1/1)") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestParseSinceCutoff(t *testing.T) {
	now := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	got, err := parseSinceCutoff("90s", now)
	if err != nil || !got.Equal(now.Add(-90*time.Second)) {
		t.Fatalf("duration cutoff wrong: %v err=%v", got, err)
	}
	got, err = parseSinceCutoff("2026-07-19T11:00:00Z", now)
	if err != nil || !got.Equal(time.Date(2026, 7, 19, 11, 0, 0, 0, time.UTC)) {
		t.Fatalf("absolute cutoff wrong: %v err=%v", got, err)
	}
	if _, err := parseSinceCutoff("-5m", now); err == nil {
		t.Fatal("negative duration must be rejected")
	}
	if _, err := parseSinceCutoff("yesterday", now); err == nil {
		t.Fatal("garbage must be rejected")
	}
}

func TestDetachJobLogScriptShapes(t *testing.T) {
	full := readDetachJobLogScript("/var/okdev/exec/j.log", jobsLogsOptions{TailLines: -1})
	for _, want := range []string{"okdev-log-size:", "mktemp", `sed -e 's/\r$//' -e 's/\r/\n/g' "$f"`} {
		if !strings.Contains(full, want) {
			t.Fatalf("full read script missing %q:\n%s", want, full)
		}
	}
	tailed := readDetachJobLogScript("/var/okdev/exec/j.log", jobsLogsOptions{TailLines: 200})
	if !strings.Contains(tailed, `| tail -n 200`) {
		t.Fatalf("tail read script missing tail stage: %s", tailed)
	}
	filtered := readDetachJobLogScript("/var/okdev/exec/j.log", jobsLogsOptions{TailLines: 5, Grep: "reward=", Dedup: true})
	for _, want := range []string{"grep -E -- 'reward='", "[repeated ", `| tail -n 5`} {
		if !strings.Contains(filtered, want) {
			t.Fatalf("filtered script missing %q:\n%s", want, filtered)
		}
	}
	grepIdx := strings.Index(filtered, "grep -E")
	dedupIdx := strings.Index(filtered, "awk")
	tailIdx := strings.Index(filtered, "| tail -n 5")
	if !(grepIdx < dedupIdx && dedupIdx < tailIdx) {
		t.Fatalf("pipeline must be grep | dedup | tail, got:\n%s", filtered)
	}
	since := readDetachJobLogScript("/var/okdev/exec/j.log", jobsLogsOptions{TailLines: -1, SinceEpoch: 1750000000})
	if !strings.Contains(since, "stat -c %Y") || !strings.Contains(since, "-lt 1750000000") {
		t.Fatalf("since gate missing: %s", since)
	}
	follow := followDetachJobLogScript("/var/okdev/exec/j.log", -1)
	if !strings.Contains(follow, "tail -n +1 -f") {
		t.Fatalf("follow script must tail whole file: %s", follow)
	}
	followTail := followDetachJobLogScript("/var/okdev/exec/j.log", 50)
	if !strings.Contains(followTail, "tail -n 50 -f") {
		t.Fatalf("follow script must honor --tail: %s", followTail)
	}
}

// Regression for issue #167: an exec stream that drops output (empty or
// truncated despite a non-empty log file) must be retried, not presented as
// the job's output.
func TestRunJobsLogsRetriesDroppedRead(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-drop", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(0)),
			},
		},
		streamPlanSeq: map[string][]fakeJobsStreamPlan{
			"okdev-sess-worker-0|read": {
				{stdout: "", stderr: "okdev-log-size:6\n"},        // dropped stream: size says 6, got 0
				{stdout: "hello\n", stderr: "okdev-log-size:6\n"}, // retry succeeds
			},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	if err := runJobsLogs(context.Background(), client, "default", pods, "dev", "job-drop", pdshDefaultFanout, jobsLogsOptions{TailLines: -1}, &out); err != nil {
		t.Fatalf("runJobsLogs: %v", err)
	}
	if !strings.Contains(out.String(), "hello") {
		t.Fatalf("expected retried content, got %q", out.String())
	}
	if strings.Count(out.String(), "hello") != 1 {
		t.Fatalf("content must not duplicate across retries, got %q", out.String())
	}
}

func TestRunJobsLogsFailsAfterPersistentTruncation(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-trunc", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(0)),
			},
		},
		streamPlans: map[string]fakeJobsStreamPlan{
			"okdev-sess-worker-0|read": {stdout: "par", stderr: "okdev-log-size:100\n"},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsLogs(context.Background(), client, "default", pods, "dev", "job-trunc", pdshDefaultFanout, jobsLogsOptions{TailLines: -1}, &out)
	if err == nil {
		t.Fatal("expected truncation error")
	}
	if !strings.Contains(out.String(), "truncated: got 3 of 100 bytes") {
		t.Fatalf("expected truncation detail in failure output, got %q / err %v", out.String(), err)
	}
	// Truncated payloads must not leak into the output stream.
	if strings.Contains(out.String(), "[worker-0] par") {
		t.Fatalf("truncated content leaked into output: %q", out.String())
	}
}

// deadContainerJobsClient wraps fakeJobsClient and fails log streams for the
// named container, modeling an OOMKilled target while the sidecar stays up.
type deadContainerJobsClient struct {
	*fakeJobsClient
	deadContainer string
	streamMu      sync.Mutex
	streamCalls   []string
}

func (c *deadContainerJobsClient) StreamShInContainer(ctx context.Context, namespace, pod, container, script string, stdout, stderr io.Writer) error {
	c.streamMu.Lock()
	c.streamCalls = append(c.streamCalls, pod+"|"+container)
	c.streamMu.Unlock()
	if container == c.deadContainer {
		return fmt.Errorf("container not found (%q)", container)
	}
	return c.fakeJobsClient.StreamShInContainer(ctx, namespace, pod, container, script, stdout, stderr)
}

func TestRunJobsLogsFallsBackToSidecarWhenContainerGone(t *testing.T) {
	client := &deadContainerJobsClient{
		fakeJobsClient: &fakeJobsClient{
			listOutputs: map[string][]string{
				"okdev-sess-worker-0": {
					detachMetadataJSON("job-oom", "okdev-sess-worker-0", "pytorch", 100, "orphaned", nil),
				},
			},
			streamPlans: map[string]fakeJobsStreamPlan{
				"okdev-sess-worker-0|read": {stdout: "last words\n", stderr: "okdev-log-size:11\n"},
			},
		},
		deadContainer: "pytorch",
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	if err := runJobsLogs(context.Background(), client, "default", pods, "pytorch", "job-oom", pdshDefaultFanout, jobsLogsOptions{TailLines: -1}, &out); err != nil {
		t.Fatalf("runJobsLogs: %v", err)
	}
	if !strings.Contains(out.String(), "last words") {
		t.Fatalf("expected log content via sidecar fallback, got %q", out.String())
	}
	wantCalls := []string{"okdev-sess-worker-0|pytorch", "okdev-sess-worker-0|" + syncthingContainerName}
	if len(client.streamCalls) != 2 || client.streamCalls[0] != wantCalls[0] || client.streamCalls[1] != wantCalls[1] {
		t.Fatalf("expected stream calls %v, got %v", wantCalls, client.streamCalls)
	}
}

type fakeJobsClient struct {
	mu              sync.Mutex
	listOutputs     map[string][]string
	listErrs        map[string][]error
	listCalls       map[string]int
	streamPlans     map[string]fakeJobsStreamPlan
	streamPlanSeq   map[string][]fakeJobsStreamPlan
	streamSeqCalls  map[string]int
	execShResponses map[string]fakeJobsExecResponse
	execScripts     []string
	onExec          func(pod, key string)
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
	if strings.Contains(script, "exec tail -n") {
		key = "follow"
	}
	var plan fakeJobsStreamPlan
	var ok bool
	if seq, seqOK := f.streamPlanSeq[pod+"|"+key]; seqOK && len(seq) > 0 {
		if f.streamSeqCalls == nil {
			f.streamSeqCalls = make(map[string]int)
		}
		idx := f.streamSeqCalls[pod+"|"+key]
		f.streamSeqCalls[pod+"|"+key]++
		if idx >= len(seq) {
			idx = len(seq) - 1
		}
		plan, ok = seq[idx], true
	} else {
		plan, ok = f.streamPlans[pod+"|"+key]
	}
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

// detachMetadataLineGroup renders the full "<alive>\t<groupLive>\t<json>"
// scan line for a job launched with a process group.
func detachMetadataLineGroup(jobID, pod, container string, pid, pgid int, state string, exitCode *int, alive bool, groupLive int) string {
	exitPart := ""
	if exitCode != nil {
		exitPart = fmt.Sprintf(",\"exitCode\":%d", *exitCode)
	}
	aliveFlag := "0"
	if alive {
		aliveFlag = "1"
	}
	return fmt.Sprintf("%s\t%d\t{\"jobId\":\"%s\",\"pod\":\"%s\",\"container\":\"%s\",\"pid\":%d,\"pgid\":%d,\"command\":\"python train.py\",\"startedAt\":\"2026-05-09T10:00:00Z\",\"stdoutPath\":\"/var/okdev/exec/%s.log\",\"stderrPath\":\"/var/okdev/exec/%s.log\",\"metaPath\":\"/var/okdev/exec/%s.json\",\"state\":\"%s\"%s}\n",
		aliveFlag, groupLive, jobID, pod, container, pid, pgid, jobID, jobID, jobID, state, exitPart)
}

// Executes the generated read script through a real shell against a fixture
// log with tqdm-style \r rewrites, CRLF endings, and multi-rank repeated
// error spam — the #188/#189 scenarios.
func TestDetachJobLogScriptFiltersThroughRealShell(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "job.log")
	content := "step 1 reward=0.10\r\n" + // CRLF line
		"10%|#\r20%|##\r30%|###\rprogress done\n" + // tqdm rewrites, one physical line
		strings.Repeat("ValueError: missing weights for layer.7\n", 5) + // per-rank spam
		"step 2 reward=0.15\n"
	if err := os.WriteFile(logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(opts jobsLogsOptions) string {
		t.Helper()
		script := readDetachJobLogScript(logPath, opts)
		cmd := exec.Command("sh", "-c", script)
		var stdout, stderr bytes.Buffer
		cmd.Stdout = &stdout
		cmd.Stderr = &stderr
		if err := cmd.Run(); err != nil {
			t.Fatalf("script failed: %v (stderr: %s)", err, stderr.String())
		}
		size, ok := parseDetachLogSize(stderr.String())
		if !ok || size != int64(stdout.Len()) {
			t.Fatalf("size marker %d does not match stdout %d (stderr: %s)", size, stdout.Len(), stderr.String())
		}
		return stdout.String()
	}

	// CR normalization: tqdm frames become separate lines, CRLF is stripped.
	plain := run(jobsLogsOptions{TailLines: -1})
	if !strings.Contains(plain, "step 1 reward=0.10\n") || !strings.Contains(plain, "30%|###\nprogress done\n") {
		t.Fatalf("CR normalization wrong:\n%q", plain)
	}

	// grep composes with tail: latest N matching lines.
	grepped := run(jobsLogsOptions{TailLines: 1, Grep: "reward="})
	if grepped != "step 2 reward=0.15\n" {
		t.Fatalf("grep+tail wrong: %q", grepped)
	}

	// dedup folds the repeated exception into one line with a count.
	deduped := run(jobsLogsOptions{TailLines: -1, Dedup: true})
	if !strings.Contains(deduped, "ValueError: missing weights for layer.7 [repeated 5x]") {
		t.Fatalf("dedup missing repeat fold:\n%q", deduped)
	}
	if strings.Count(deduped, "ValueError") != 1 {
		t.Fatalf("dedup left duplicate lines:\n%q", deduped)
	}
	if !strings.Contains(deduped, "step 2 reward=0.15") {
		t.Fatalf("dedup must keep unique lines:\n%q", deduped)
	}
}

func TestRunJobsWaitGrepReturnsOnPatternWhileRunning(t *testing.T) {
	oldPollEvery := detachJobPollEvery
	detachJobPollEvery = 5 * time.Millisecond
	defer func() { detachJobPollEvery = oldPollEvery }()

	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-metric", "okdev-sess-worker-0", "dev", 100, "running", nil),
			},
		},
		streamPlanSeq: map[string][]fakeJobsStreamPlan{
			"okdev-sess-worker-0|read": {
				{stdout: "", stderr: "okdev-log-size:0\n"},                      // no match yet
				{stdout: "step 2 reward=0.15\n", stderr: "okdev-log-size:19\n"}, // pattern appeared
			},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-metric", pdshDefaultFanout, jobsWaitOptions{Grep: "reward=", TailLines: 1}, &out)
	if err != nil {
		t.Fatalf("runJobsWait --grep: %v", err)
	}
	if !strings.Contains(out.String(), "reward=0.15") {
		t.Fatalf("expected the matching line to be printed, got %q", out.String())
	}
}

func TestRunJobsWaitGrepMatchesAfterExitAndOnFailure(t *testing.T) {
	// The pattern probe runs before the terminal check: a job that exits
	// (even non-zero) with the pattern in its log still satisfies the wait —
	// waiting for an Error line is a first-class use.
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-err", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(1)),
			},
		},
		streamPlans: map[string]fakeJobsStreamPlan{
			"okdev-sess-worker-0|read": {stdout: "RuntimeError: boom\n", stderr: "okdev-log-size:19\n"},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-err", pdshDefaultFanout, jobsWaitOptions{Grep: "Error", TailLines: -1}, &out)
	if err != nil {
		t.Fatalf("runJobsWait --grep on exited job: %v", err)
	}
	if !strings.Contains(out.String(), "RuntimeError: boom") {
		t.Fatalf("expected error line printed, got %q", out.String())
	}
}

func TestRunJobsWaitGrepFailsWhenJobEndsWithoutMatch(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-quiet", "okdev-sess-worker-0", "dev", 100, "exited", intPtr(0)),
			},
		},
		streamPlans: map[string]fakeJobsStreamPlan{
			"okdev-sess-worker-0|read": {stdout: "", stderr: "okdev-log-size:0\n"},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsWait(context.Background(), client, "default", pods, "dev", "job-quiet", pdshDefaultFanout, jobsWaitOptions{Grep: "reward=", TailLines: 1}, &out)
	if err == nil || !strings.Contains(err.Error(), "without matching --grep") {
		t.Fatalf("expected no-match failure, got %v", err)
	}
}
