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

func TestRunJobsStopSignalsLingeringGroupAfterLeaderExit(t *testing.T) {
	// The leader (torchrun) already exited but two group members
	// (pretrain.py workers) linger holding GPU memory. Stop must signal the
	// group even though the metadata state is terminal, and only report
	// success once the group is empty.
	oldPollEvery := detachJobPollEvery
	oldStopGrace := detachJobStopGrace
	detachJobPollEvery = 5 * time.Millisecond
	detachJobStopGrace = 15 * time.Millisecond
	defer func() {
		detachJobPollEvery = oldPollEvery
		detachJobStopGrace = oldStopGrace
	}()

	exit1 := 1
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataLineGroup("job-shared", "okdev-sess-worker-0", "pytorch", 100, 100, "exited", &exit1, false, 2),
				detachMetadataLineGroup("job-shared", "okdev-sess-worker-0", "pytorch", 100, 100, "exited", &exit1, false, 0),
			},
		},
		execShResponses: map[string]fakeJobsExecResponse{
			"okdev-sess-worker-0|TERM": {stdout: "signaled group (2 members)\n"},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	if err := runJobsStop(context.Background(), client, "default", pods, "pytorch", "job-shared", pdshDefaultFanout, &out); err != nil {
		t.Fatalf("runJobsStop: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "sent SIGTERM pgid=100 (signaled group (2 members))") {
		t.Fatalf("expected group TERM line, got %q", got)
	}
	if len(client.execScripts) != 1 || !strings.Contains(client.execScripts[0], `kill -TERM -- "-$pgid"`) {
		t.Fatalf("expected a single group TERM script, got %+v", client.execScripts)
	}
}

func TestRunJobsStopFailsWhileGroupMembersRemain(t *testing.T) {
	// If the group never drains (e.g. processes stuck in D-state), stop must
	// escalate to SIGKILL and still exit non-zero rather than reporting a
	// clean stop.
	oldPollEvery := detachJobPollEvery
	oldStopGrace := detachJobStopGrace
	detachJobPollEvery = 5 * time.Millisecond
	detachJobStopGrace = 15 * time.Millisecond
	defer func() {
		detachJobPollEvery = oldPollEvery
		detachJobStopGrace = oldStopGrace
	}()

	exit1 := 1
	client := &fakeJobsClient{
		listOutputs: map[string][]string{
			"okdev-sess-worker-0": {
				detachMetadataLineGroup("job-shared", "okdev-sess-worker-0", "pytorch", 100, 100, "exited", &exit1, false, 2),
			},
		},
		execShResponses: map[string]fakeJobsExecResponse{
			"okdev-sess-worker-0|TERM": {stdout: "signaled group (2 members)\n"},
			"okdev-sess-worker-0|KILL": {stdout: "signaled group (2 members)\n"},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
	var out bytes.Buffer
	err := runJobsStop(context.Background(), client, "default", pods, "pytorch", "job-shared", pdshDefaultFanout, &out)
	if err == nil || !strings.Contains(err.Error(), "still has running pods") {
		t.Fatalf("expected still-running error, got %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "sent SIGKILL pgid=100") {
		t.Fatalf("expected group KILL escalation, got %q", got)
	}
}

func TestStopRowNeedsSignalAndSettled(t *testing.T) {
	running := execJobView{State: "running"}
	exitedClean := execJobView{State: "exited"}
	exitedWithGroup := execJobView{State: "exited", PGID: 100, GroupLive: 3}
	exitedGroupDrained := execJobView{State: "exited", PGID: 100, GroupLive: 0}
	if !stopRowNeedsSignal(running) || !stopRowNeedsSignal(exitedWithGroup) {
		t.Fatal("running rows and lingering groups must be signaled")
	}
	if stopRowNeedsSignal(exitedClean) || stopRowNeedsSignal(exitedGroupDrained) {
		t.Fatal("terminal rows without live group members must not be signaled")
	}
	if stopJobSettled(logicalExecJobView{PodStates: []execJobView{exitedClean, exitedWithGroup}}) {
		t.Fatal("job with lingering group members is not settled")
	}
	if !stopJobSettled(logicalExecJobView{PodStates: []execJobView{exitedClean, exitedGroupDrained}}) {
		t.Fatal("job with drained groups is settled")
	}
}

func TestSignalDetachJobScriptGroupAndFallback(t *testing.T) {
	group := signalDetachJobScript("job-1", 100, 100, "TERM")
	for _, want := range []string{
		`kill -TERM -- "-$pgid"`,
		"/proc/[0-9]*/stat",
		"OKDEV_JOB_ID=job-1",
		// Legacy leader path stays as the fallback tail when the group is
		// already empty or belongs to someone else.
		"kill -TERM \"$pid\"",
	} {
		if !strings.Contains(group, want) {
			t.Fatalf("expected group script to contain %q, got %q", want, group)
		}
	}
	legacy := signalDetachJobScript("job-1", 100, 0, "TERM")
	if strings.Contains(legacy, "pgid") {
		t.Fatalf("legacy script must not reference pgid, got %q", legacy)
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
