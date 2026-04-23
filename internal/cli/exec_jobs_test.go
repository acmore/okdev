package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
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
	err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false)
	if err == nil {
		t.Fatal("expected error reflecting partial failure")
	}
	if !strings.Contains(err.Error(), "1 of 2 pods failed") {
		t.Fatalf("unexpected error: %v", err)
	}
	got := out.String()
	// Healthy pod row must still render.
	for _, want := range []string{"okdev-sess-worker-0", "job-a", "running"} {
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
			"okdev-sess-worker-0": "{\"jobId\":\"job-a\",\"pod\":\"okdev-sess-worker-0\",\"container\":\"dev\",\"pid\":48217,\"command\":\"python train.py\",\"startedAt\":\"2026-04-23T03:00:00Z\",\"stdoutPath\":\"/tmp/okdev-exec/job-a.log\",\"stderrPath\":\"/tmp/okdev-exec/job-a.log\",\"metaPath\":\"/tmp/okdev-exec/job-a.json\",\"state\":\"running\"}\n",
			"okdev-sess-worker-1": "",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var out bytes.Buffer
	if err := runExecJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false); err != nil {
		t.Fatalf("runExecJobs: %v", err)
	}
	got := out.String()
	for _, want := range []string{"POD", "JOB ID", "STATE", "okdev-sess-worker-0", "job-a", "running"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected table output containing %q, got %q", want, got)
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
		"grep -qx \"OKDEV_JOB_ID=$job_id\"",
		// Per-line output is "<alive>\t<json>".
		"printf '%s\\t%s\\n' \"$alive\" \"$contents\"",
		// Best-effort cleanup of stale temp files left by killed wrappers.
		"-name '*.tmp'",
		"-mmin +1",
	} {
		if !strings.Contains(cmd[2], want) {
			t.Fatalf("expected detach jobs command to contain %q, got %q", want, cmd[2])
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
