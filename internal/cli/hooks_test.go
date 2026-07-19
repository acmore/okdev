package cli

import (
	"bytes"
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestComputeHookState(t *testing.T) {
	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	pod := func(annotations map[string]string, containerStart time.Time) kube.PodSummary {
		summary := kube.PodSummary{Name: "w-0", Annotations: annotations}
		if !containerStart.IsZero() {
			summary.ContainerStarts = []kube.ContainerStart{{Name: "dev", StartedAt: containerStart}}
		}
		return summary
	}
	cases := []struct {
		name        string
		annotations map[string]string
		start       time.Time
		want        string
	}{
		{name: "no annotations is pending", want: hookStatePending},
		{name: "legacy done marker reads done", annotations: map[string]string{annotationPostSyncDone: "true"}, want: hookStateDone},
		{
			name: "done before container start is stale",
			annotations: map[string]string{
				annotationPostSyncDone:  "true",
				annotationPostSyncState: hookStateDone,
				annotationPostSyncAt:    base.Format(time.RFC3339),
			},
			start: base.Add(5 * time.Minute),
			want:  hookStateStale,
		},
		{
			name: "done after container start stays done",
			annotations: map[string]string{
				annotationPostSyncDone:  "true",
				annotationPostSyncState: hookStateDone,
				annotationPostSyncAt:    base.Format(time.RFC3339),
			},
			start: base.Add(-5 * time.Minute),
			want:  hookStateDone,
		},
		{
			name:        "running state reads running",
			annotations: map[string]string{annotationPostSyncState: hookStateRunning, annotationPostSyncAt: base.Format(time.RFC3339)},
			start:       base.Add(-time.Minute),
			want:        hookStateRunning,
		},
		{
			name:        "failed state reads failed",
			annotations: map[string]string{annotationPostSyncState: hookStateFailed, annotationPostSyncAt: base.Format(time.RFC3339)},
			start:       base.Add(-time.Minute),
			want:        hookStateFailed,
		},
		{
			name:        "unparseable timestamp cannot go stale",
			annotations: map[string]string{annotationPostSyncDone: "true", annotationPostSyncState: hookStateDone, annotationPostSyncAt: "garbage"},
			start:       base.Add(5 * time.Minute),
			want:        hookStateDone,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			state, _ := computeHookState(pod(tc.annotations, tc.start), postSyncHook, "dev")
			if state != tc.want {
				t.Fatalf("state = %q, want %q", state, tc.want)
			}
		})
	}
}

func TestHookNeedsRun(t *testing.T) {
	for state, want := range map[string]bool{
		hookStatePending: true,
		hookStateRunning: true,
		hookStateFailed:  true,
		hookStateStale:   true,
		hookStateDone:    false,
	} {
		if got := hookNeedsRun(state); got != want {
			t.Fatalf("hookNeedsRun(%q) = %v, want %v", state, got, want)
		}
	}
}

// fakeHookConvergeClient mutates pod annotations on AnnotatePodMap so the
// convergence loop observes hooks completing, and can grow the pod list to
// model a controller creating a replacement pod mid-run.
type fakeHookConvergeClient struct {
	mu        sync.Mutex
	pods      map[string]*kube.PodSummary
	latePod   string
	listCalls int
	execCalls []string
}

func (f *fakeHookConvergeClient) ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listCalls++
	if f.latePod != "" && f.listCalls == 2 {
		f.pods[f.latePod] = &kube.PodSummary{Name: f.latePod, Phase: "Running", Annotations: map[string]string{}}
	}
	out := make([]kube.PodSummary, 0, len(f.pods))
	for _, pod := range f.pods {
		out = append(out, *pod)
	}
	return out, nil
}

func (f *fakeHookConvergeClient) GetPodSummary(_ context.Context, _ string, pod string) (*kube.PodSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if summary, ok := f.pods[pod]; ok {
		copy := *summary
		return &copy, nil
	}
	return nil, context.Canceled
}

func (f *fakeHookConvergeClient) ExecShInContainer(_ context.Context, _ string, pod, _ string, _ string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.execCalls = append(f.execCalls, pod)
	return nil, nil
}

func (f *fakeHookConvergeClient) AnnotatePodMap(_ context.Context, _ string, pod string, annotations map[string]string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	summary, ok := f.pods[pod]
	if !ok {
		return nil
	}
	if summary.Annotations == nil {
		summary.Annotations = map[string]string{}
	}
	for k, v := range annotations {
		summary.Annotations[k] = v
	}
	return nil
}

func TestWaitForPostSyncConvergedCoversLateCreatedPod(t *testing.T) {
	// w-0 starts pending so the loop runs a fanout; w-1 is created by the
	// "controller" only once the fanout re-lists pods — the case a plain
	// one-shot fanout would miss.
	client := &fakeHookConvergeClient{
		pods: map[string]*kube.PodSummary{
			"w-0": {Name: "w-0", Phase: "Running", Annotations: map[string]string{}},
		},
		latePod: "w-1",
	}
	var warnings bytes.Buffer
	err := waitForPostSyncConverged(context.Background(), client, "default",
		map[string]string{"okdev.io/managed": "true", "okdev.io/session": "sess"},
		"dev", "pip install -e .", 10*time.Second, &warnings, nil)
	if err != nil {
		t.Fatalf("waitForPostSyncConverged: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	joined := strings.Join(client.execCalls, ",")
	if !strings.Contains(joined, "w-1") || !strings.Contains(joined, "w-0") {
		t.Fatalf("expected postSync on both pods including the late one, exec calls: %v", client.execCalls)
	}
	for _, pod := range []string{"w-0", "w-1"} {
		if state, _ := computeHookState(*client.pods[pod], postSyncHook, "dev"); state != hookStateDone {
			t.Fatalf("pod %s should end done, got %q", pod, state)
		}
	}
}

func TestWaitForPostSyncConvergedReturnsImmediatelyWhenAllDone(t *testing.T) {
	client := &fakeHookConvergeClient{
		pods: map[string]*kube.PodSummary{
			"w-0": {Name: "w-0", Phase: "Running", Annotations: map[string]string{annotationPostSyncDone: "true"}},
		},
	}
	var warnings bytes.Buffer
	err := waitForPostSyncConverged(context.Background(), client, "default",
		map[string]string{"okdev.io/managed": "true", "okdev.io/session": "sess"},
		"dev", "pip install -e .", 10*time.Second, &warnings, nil)
	if err != nil {
		t.Fatalf("waitForPostSyncConverged: %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.execCalls) != 0 {
		t.Fatalf("expected no hook runs when everything is done, got %v", client.execCalls)
	}
}

func TestPendingPostSyncPodsSkipsDeletingAndDone(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Annotations: map[string]string{annotationPostSyncDone: "true"}},
		{Name: "b"},
		{Name: "c", Deleting: true},
	}
	pending := pendingPostSyncPods(pods, "dev")
	if len(pending) != 1 || pending[0] != "b" {
		t.Fatalf("pending = %v, want [b]", pending)
	}
}
