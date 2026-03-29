package workload

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type retryWaitClient struct {
	podLister
	errs  []error
	calls int
}

func (r *retryWaitClient) WaitReadyWithProgress(_ context.Context, _ string, _ string, _ time.Duration, _ func(kube.PodReadinessProgress)) error {
	r.calls++
	if r.calls > len(r.errs) {
		return nil
	}
	return r.errs[r.calls-1]
}

type staticPodLister struct {
	pods []kube.PodSummary
}

func (s staticPodLister) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return s.pods, nil
}

func TestWaitForCandidatePodReadyRetriesOnDeletedCandidate(t *testing.T) {
	client := &retryWaitClient{
		podLister: staticPodLister{pods: []kube.PodSummary{{Name: "target-1"}}},
		errs:      []error{errors.New("pod/target-1 was deleted while waiting for readiness")},
	}
	selectCandidate := func(ctx context.Context, k podLister, namespace string) (TargetRef, []kube.PodSummary, error) {
		pods, err := k.ListPods(ctx, namespace, false, "")
		if err != nil {
			return TargetRef{}, nil, err
		}
		return TargetRef{PodName: "target-1"}, pods, nil
	}

	if err := waitForCandidatePodReady(context.Background(), client, "default", selectCandidate, time.Second, nil, "timed out"); err != nil {
		t.Fatalf("waitForCandidatePodReady: %v", err)
	}
	if client.calls != 2 {
		t.Fatalf("expected retry after deleted candidate, got %d wait calls", client.calls)
	}
}

func TestResolveManifestPathUsesProjectRootForFolderConfig(t *testing.T) {
	configPath := filepath.Join("/tmp", "repo", ".okdev", "okdev.yaml")
	manifestPath := ".okdev/pytorchjob.yaml"

	got := ResolveManifestPath(configPath, manifestPath)
	want := filepath.Join("/tmp", "repo", ".okdev", "pytorchjob.yaml")
	if got != want {
		t.Fatalf("expected %q, got %q", want, got)
	}
}
