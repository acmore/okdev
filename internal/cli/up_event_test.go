package cli

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type fakeSessionPodEventWatcher struct {
	mu        sync.Mutex
	listCalls int
	podsByCall [][]kube.PodSummary
	watched   []string
}

func (f *fakeSessionPodEventWatcher) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	idx := f.listCalls
	f.listCalls++
	if idx >= len(f.podsByCall) {
		idx = len(f.podsByCall) - 1
	}
	if idx < 0 {
		return nil, nil
	}
	return f.podsByCall[idx], nil
}

func (f *fakeSessionPodEventWatcher) WatchPodEvents(_ context.Context, _ string, pod string, onEvent func(string, string)) {
	f.mu.Lock()
	f.watched = append(f.watched, pod)
	f.mu.Unlock()
	onEvent("Pulling", "pulling image")
}

func TestWatchSessionPodEventsTracksNewPods(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fake := &fakeSessionPodEventWatcher{
		podsByCall: [][]kube.PodSummary{
			{{Name: "trainer-master-0"}},
			{{Name: "trainer-master-0"}, {Name: "trainer-worker-0"}},
		},
	}

	var mu sync.Mutex
	var got []string
	done := make(chan struct{})
	go func() {
		watchSessionPodEvents(ctx, fake, "default", "sess", 5*time.Millisecond, func(pod, reason, message string) {
			mu.Lock()
			got = append(got, pod+":"+reason+":"+message)
			count := len(got)
			mu.Unlock()
			if count >= 2 {
				cancel()
			}
		})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
		t.Fatal("timed out waiting for pod event watcher")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(got) != 2 {
		t.Fatalf("expected 2 events, got %#v", got)
	}
	if got[0] != "trainer-master-0:Pulling:pulling image" {
		t.Fatalf("unexpected first event %q", got[0])
	}
	if got[1] != "trainer-worker-0:Pulling:pulling image" {
		t.Fatalf("unexpected second event %q", got[1])
	}
}
