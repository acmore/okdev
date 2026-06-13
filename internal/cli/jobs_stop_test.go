package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestRunJobsStopEscalatesToSIGKILLAfterGrace(t *testing.T) {
	oldPollEvery := detachJobPollEvery
	oldStopGrace := detachJobStopGrace
	detachJobPollEvery = 5 * time.Millisecond
	detachJobStopGrace = 15 * time.Millisecond
	defer func() {
		detachJobPollEvery = oldPollEvery
		detachJobStopGrace = oldStopGrace
	}()

	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "running", nil),
			},
		},
		execShResponses: map[string]fakeJobsExecResponse{
			"okdev-sess-worker-0|TERM": {stdout: "signaled\n"},
			"okdev-sess-worker-0|KILL": {stdout: "signaled\n"},
		},
	}
	client.onExec = func(pod, key string) {
		if key != "KILL" {
			return
		}
		client.mu.Lock()
		defer client.mu.Unlock()
		client.listOutputs[pod] = append(client.listOutputs[pod], detachMetadataJSON("job-shared", pod, "dev", 100, "exited", intPtr(137)))
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	if err := runJobsStop(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, &out); err != nil {
		t.Fatalf("runJobsStop: %v", err)
	}
	got := out.String()
	for _, want := range []string{"sent SIGTERM", "sent SIGKILL"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
	if len(client.execScripts) < 2 || !strings.Contains(client.execScripts[0], "kill -TERM") || !strings.Contains(client.execScripts[1], "kill -KILL") {
		t.Fatalf("expected TERM then KILL, got %+v", client.execScripts)
	}
}

func TestRunJobsStopReturnsPartialFailure(t *testing.T) {
	oldPollEvery := detachJobPollEvery
	oldStopGrace := detachJobStopGrace
	detachJobPollEvery = 5 * time.Millisecond
	detachJobStopGrace = 15 * time.Millisecond
	defer func() {
		detachJobPollEvery = oldPollEvery
		detachJobStopGrace = oldStopGrace
	}()

	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataJSON("job-shared", "okdev-sess-worker-0", "dev", 100, "running", nil),
			},
		},
		execShResponses: map[string]fakeJobsExecResponse{
			"okdev-sess-worker-0|TERM": {err: errors.New("permission denied")},
			"okdev-sess-worker-0|KILL": {err: errors.New("permission denied")},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsStop(context.Background(), client, "default", pods, "dev", "job-shared", pdshDefaultFanout, &out)
	if err == nil {
		t.Fatal("expected failure")
	}
	got := out.String()
	if !strings.Contains(got, "FAILED:") || !strings.Contains(got, "permission denied") {
		t.Fatalf("expected failure footer, got %q", got)
	}
}
