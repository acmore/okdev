package cli

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

type fakePostSyncClient struct {
	mu            sync.Mutex
	pods          []kube.PodSummary
	annotations   map[string]string
	execErr       map[string]error
	execCalls     []string
	annotateCalls []string
	execCtxErr    error
}

func (f *fakePostSyncClient) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return f.pods, nil
}

func (f *fakePostSyncClient) GetPodSummary(_ context.Context, _ string, pod string) (*kube.PodSummary, error) {
	for _, item := range f.pods {
		if item.Name == pod {
			copy := item
			return &copy, nil
		}
	}
	return nil, errors.New("pod not found")
}

func (f *fakePostSyncClient) GetPodAnnotation(_ context.Context, _ string, pod, key string) (string, error) {
	if key != "okdev.io/post-sync-done" {
		return "", nil
	}
	return f.annotations[pod], nil
}

func (f *fakePostSyncClient) ExecShInContainer(ctx context.Context, _ string, pod, _ string, _ string) ([]byte, error) {
	f.mu.Lock()
	f.execCalls = append(f.execCalls, pod)
	if err := ctx.Err(); err != nil {
		f.execCtxErr = err
		f.mu.Unlock()
		return nil, err
	}
	if err := f.execErr[pod]; err != nil {
		f.mu.Unlock()
		return nil, err
	}
	f.mu.Unlock()
	return nil, nil
}

func (f *fakePostSyncClient) AnnotatePod(_ context.Context, _ string, pod, key, value string) error {
	f.mu.Lock()
	f.annotateCalls = append(f.annotateCalls, pod+":"+key+"="+value)
	f.mu.Unlock()
	return nil
}

func TestRunPostSyncOnAllPodsSummarizesSharedWorkspaceFanout(t *testing.T) {
	client := &fakePostSyncClient{
		pods: []kube.PodSummary{
			{Name: "worker-a", Phase: "Running"},
			{Name: "worker-b", Phase: "Running"},
			{Name: "worker-c", Phase: "Running"},
			{Name: "worker-d", Phase: "Running", Deleting: true},
		},
		annotations: map[string]string{
			"worker-b": "true",
		},
	}

	var warnings bytes.Buffer
	summary, err := runPostSyncOnAllPods(
		context.Background(),
		client,
		"default",
		map[string]string{"okdev.io/managed": "true", "okdev.io/session": "sess"},
		"dev",
		"pip install -e .",
		&warnings,
	)
	if err != nil {
		t.Fatalf("runPostSyncOnAllPods() error = %v", err)
	}
	if summary.Ran != 2 || summary.Skipped != 1 || summary.NotRunning != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.execCalls) != 2 {
		t.Fatalf("expected 2 exec calls, got %#v", client.execCalls)
	}
	if len(client.annotateCalls) != 2 {
		t.Fatalf("expected 2 annotate calls, got %#v", client.annotateCalls)
	}
	if warnings.Len() != 0 {
		t.Fatalf("expected no warnings, got %q", warnings.String())
	}
}

func TestRunPostSyncIfNeededHonorsParentCancellation(t *testing.T) {
	client := &fakePostSyncClient{
		pods:        []kube.PodSummary{{Name: "worker-a", Phase: "Running"}},
		annotations: map[string]string{},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var warnings bytes.Buffer
	ran, err := runPostSyncIfNeeded(ctx, client, "default", "worker-a", "dev", "pip install -e .", &warnings)
	if !ran {
		t.Fatal("expected runPostSyncIfNeeded to report attempted execution")
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation, got %v", err)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if !errors.Is(client.execCtxErr, context.Canceled) {
		t.Fatalf("expected exec context cancellation, got %v", client.execCtxErr)
	}
	if len(client.annotateCalls) != 0 {
		t.Fatalf("expected no annotations after cancellation, got %#v", client.annotateCalls)
	}
}
