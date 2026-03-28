package cli

import (
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestBuildSessionViewsGroupsPodsAndPrefersAnnotatedTarget(t *testing.T) {
	now := time.Now()
	views := buildSessionViews([]kube.PodSummary{
		{
			Namespace: "default",
			Name:      "job-worker-0",
			Phase:     "Running",
			Ready:     "1/1",
			CreatedAt: now.Add(-2 * time.Minute),
			Labels: map[string]string{
				"okdev.io/session":       "sess1",
				"okdev.io/owner":         "alice",
				"okdev.io/workload-type": "job",
			},
		},
		{
			Namespace: "default",
			Name:      "job-master-0",
			Phase:     "Running",
			Ready:     "1/1",
			CreatedAt: now.Add(-3 * time.Minute),
			Labels: map[string]string{
				"okdev.io/session":       "sess1",
				"okdev.io/owner":         "alice",
				"okdev.io/workload-type": "job",
			},
			Annotations: map[string]string{
				"okdev.io/last-attach": now.UTC().Format(time.RFC3339),
			},
		},
	})
	if len(views) != 1 {
		t.Fatalf("expected 1 session view, got %d", len(views))
	}
	view := views[0]
	if view.TargetPod != "job-master-0" {
		t.Fatalf("expected annotated target pod, got %q", view.TargetPod)
	}
	if view.WorkloadType != "job" {
		t.Fatalf("expected workload type job, got %q", view.WorkloadType)
	}
	if view.PodCount != 2 {
		t.Fatalf("expected pod count 2, got %d", view.PodCount)
	}
}

func TestSelectTargetPodReturnsFalseForEmptySlice(t *testing.T) {
	target, ok := selectTargetPod(nil)
	if ok {
		t.Fatalf("expected empty selection, got %+v", target)
	}
}

func TestSelectTargetPodFallsBackToFirstPodWithoutAnnotation(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "pod-a"},
		{Name: "pod-b"},
	}
	target, ok := selectTargetPod(pods)
	if !ok {
		t.Fatal("expected a target pod")
	}
	if target.Name != "pod-a" {
		t.Fatalf("expected first pod fallback, got %q", target.Name)
	}
}
