package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestJobsReadyRequiresLiveJobAndExactIdentity(t *testing.T) {
	running := detachMetadataJSON("job-new", "pod", "dev", 100, "running", nil)
	exited := detachMetadataJSON("job-new", "pod", "dev", 100, "exited", intPtr(0))
	replaced := detachMetadataJSON("job-new", "pod", "dev", 101, "running", nil)
	for _, tc := range []struct {
		name   string
		states []string
		probe  fakeJobsStreamPlan
		want   string
	}{
		{"healthy", []string{running}, fakeJobsStreamPlan{stdout: "job-new\n"}, ""},
		{"old instance", []string{running}, fakeJobsStreamPlan{stdout: "job-old\n"}, "expected job ID"},
		{"exit before probe", []string{exited}, fakeJobsStreamPlan{stdout: "job-new"}, "not running"},
		{"exit during probe", []string{running, running, exited}, fakeJobsStreamPlan{stdout: "job-new"}, "not running"},
		{"process replaced", []string{running, running, replaced}, fakeJobsStreamPlan{stdout: "job-new"}, "identity changed"},
		{"unhealthy correct identity", []string{running}, fakeJobsStreamPlan{stdout: "job-new", err: errors.New("exit 1")}, "probe failed"},
		{"hung probe", []string{running}, fakeJobsStreamPlan{waitForCancel: true}, "probe failed"},
		{"oversized output", []string{running}, fakeJobsStreamPlan{stdout: "job-new" + strings.Repeat(" ", 5000)}, "expected job ID"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &fakeJobsClient{listOutputs: map[string][]string{"pod": tc.states}, streamPlans: map[string]fakeJobsStreamPlan{"pod|read": tc.probe}}
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			_, err := runJobsReady(ctx, client, "ns", []kube.PodSummary{{Name: "pod"}}, "dev", "job-new", jobsReadyOptions{Probe: "health", ProbeTimeout: 5 * time.Millisecond, PollInterval: time.Millisecond})
			if tc.want == "" {
				if err != nil {
					t.Fatal(err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v, want %s", err, tc.want)
			}
		})
	}
}

func TestJobsReadyAllMembersAndCancellation(t *testing.T) {
	client := &fakeJobsClient{listOutputs: map[string][]string{}, streamPlans: map[string]fakeJobsStreamPlan{}}
	pods := []kube.PodSummary{{Name: "a"}, {Name: "b"}}
	for _, pod := range pods {
		client.listOutputs[pod.Name] = []string{detachMetadataJSON("job", pod.Name, "dev", 100, "running", nil)}
		client.streamPlans[pod.Name+"|read"] = fakeJobsStreamPlan{stdout: "job"}
	}
	client.streamPlans["b|read"] = fakeJobsStreamPlan{stdout: "old"}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	_, err := runJobsReady(ctx, client, "ns", pods, "dev", "job", jobsReadyOptions{Probe: "health", ProbeTimeout: time.Second, PollInterval: time.Millisecond})
	cancel()
	if err == nil {
		t.Fatal("one member satisfied readiness for both")
	}
	ctx, cancel = context.WithCancel(context.Background())
	cancel()
	_, err = runJobsReady(ctx, client, "ns", pods, "dev", "job", jobsReadyOptions{Probe: "health", ProbeTimeout: time.Second, PollInterval: time.Millisecond})
	if err == nil {
		t.Fatal("cancelled wait succeeded")
	}
}

func TestJobsReadyFlagValidation(t *testing.T) {
	for _, args := range [][]string{{"", "--probe", "true"}, {"id"}, {"id", "--probe", "true", "--timeout", "0s"}, {"id", "--probe", "true", "--probe-timeout", "-1s"}} {
		cmd := newJobsReadyCmd(&Options{})
		cmd.SetArgs(args)
		if err := cmd.Execute(); err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
}

func TestJobsReadyZeroPollIntervalDoesNotSpin(t *testing.T) {
	client := &fakeJobsClient{
		listOutputs: map[string][]string{"pod": {detachMetadataJSON("job", "pod", "dev", 100, "running", nil)}},
		streamPlans: map[string]fakeJobsStreamPlan{"pod|read": {stdout: "a-different-job"}},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	_, err := runJobsReady(ctx, client, "ns", []kube.PodSummary{{Name: "pod"}}, "dev", "job", jobsReadyOptions{Probe: "health", ProbeTimeout: time.Second})
	if err == nil {
		t.Fatal("a probe returning another job's ID satisfied readiness")
	}
	client.mu.Lock()
	calls := client.listCalls["pod"]
	client.mu.Unlock()
	if calls > 20 {
		t.Fatalf("zero poll interval spun the identity fanout %d times", calls)
	}
}

func TestJobsReadyDoesNotRepeatTheIdentityFanoutPerPoll(t *testing.T) {
	client := &fakeJobsClient{listOutputs: map[string][]string{}, streamPlans: map[string]fakeJobsStreamPlan{}}
	pods := []kube.PodSummary{{Name: "a"}, {Name: "b"}}
	for _, pod := range pods {
		client.listOutputs[pod.Name] = []string{detachMetadataJSON("job", pod.Name, "dev", 100, "running", nil)}
		client.streamPlans[pod.Name+"|read"] = fakeJobsStreamPlan{stdout: "job"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := runJobsReady(ctx, client, "ns", pods, "dev", "job", jobsReadyOptions{Probe: "health", ProbeTimeout: time.Second}); err != nil {
		t.Fatalf("matching probes did not satisfy readiness: %v", err)
	}
	// One poll costs the entry query, one before the member loop, and one after
	// each member's probe. Nothing repeats that fanout again after the loop.
	client.mu.Lock()
	defer client.mu.Unlock()
	for _, pod := range pods {
		if got := client.listCalls[pod.Name]; got != 4 {
			t.Fatalf("pod %s saw %d identity fanouts in one poll, want 4", pod.Name, got)
		}
	}
}
