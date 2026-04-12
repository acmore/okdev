package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestFilterPodsByRole(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "master-0", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "worker-1", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}
	got := filterPodsByRole(pods, "worker")
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-1" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestFilterPodsByLabels(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Labels: map[string]string{"gpu": "a100", "zone": "us"}},
		{Name: "b", Labels: map[string]string{"gpu": "a100", "zone": "eu"}},
		{Name: "c", Labels: map[string]string{"gpu": "h100", "zone": "us"}},
	}
	got := filterPodsByLabels(pods, []string{"gpu=a100", "zone=us"})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected pod a, got %v", got)
	}
}

func TestFilterPodsByName(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
		{Name: "worker-1"},
		{Name: "master-0"},
	}
	got := filterPodsByName(pods, []string{"worker-0", "master-0"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
}

func TestFilterPodsByNameMissing(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
	}
	got := filterPodsByName(pods, []string{"worker-0", "no-such-pod"})
	if len(got) != 1 {
		t.Fatalf("expected 1 pod, got %d", len(got))
	}
}

func TestExcludePods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
		{Name: "worker-1"},
		{Name: "worker-2"},
	}
	got := excludePods(pods, []string{"worker-1"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-2" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestDetachCommand(t *testing.T) {
	got := detachCommand("python train.py --epochs 100")
	expected := []string{"sh", "-c", "nohup sh -c 'python train.py --epochs 100' >/dev/null 2>&1 &"}
	if len(got) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(got), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("arg %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func TestDetachCommandWithQuotes(t *testing.T) {
	got := detachCommand("echo 'hello world'")
	expected := []string{"sh", "-c", "nohup sh -c 'echo '\\''hello world'\\''' >/dev/null 2>&1 &"}
	if len(got) != len(expected) {
		t.Fatalf("expected %d args, got %d: %v", len(expected), len(got), got)
	}
	for i := range got {
		if got[i] != expected[i] {
			t.Fatalf("arg %d: expected %q, got %q", i, expected[i], got[i])
		}
	}
}

func TestFilterRunningPods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Phase: "Running"},
		{Name: "b", Phase: "Pending"},
		{Name: "c", Phase: "Running"},
	}
	got := filterRunningPods(pods)
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
}
