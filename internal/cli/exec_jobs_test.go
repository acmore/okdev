package cli

import (
	"bytes"
	"context"
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
	rows, err := collectDetachJobs(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout)
	if err != nil {
		t.Fatalf("collectDetachJobs: %v", err)
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
	rows, err := collectDetachJobs(context.Background(), client, "default", pods, "dev", "job-b", pdshDefaultFanout)
	if err != nil {
		t.Fatalf("collectDetachJobs: %v", err)
	}
	if len(rows) != 1 || rows[0].JobID != "job-b" {
		t.Fatalf("expected only job-b, got %+v", rows)
	}
}
