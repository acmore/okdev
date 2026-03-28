package cli

import (
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestResolveTargetCandidateByRolePrefersEligiblePod(t *testing.T) {
	now := time.Now()
	view := sessionView{
		Session: "sess1",
		Pods: []kube.PodSummary{
			{
				Name:      "worker-0",
				CreatedAt: now.Add(-time.Minute),
				Labels: map[string]string{
					"okdev.io/workload-role": "Worker",
					"okdev.io/attachable":    "false",
				},
			},
			{
				Name:      "master-0",
				Phase:     "Running",
				Ready:     "1/1",
				CreatedAt: now,
				Labels: map[string]string{
					"okdev.io/workload-role": "Master",
					"okdev.io/attachable":    "true",
				},
			},
		},
	}

	got, role, err := resolveTargetCandidate(view, "", "Master")
	if err != nil {
		t.Fatalf("resolveTargetCandidate: %v", err)
	}
	if got.Name != "master-0" {
		t.Fatalf("expected master-0, got %q", got.Name)
	}
	if role != "Master" {
		t.Fatalf("expected role Master, got %q", role)
	}
}

func TestResolveTargetCandidateByPodRejectsNonAttachablePod(t *testing.T) {
	view := sessionView{
		Session: "sess1",
		Pods: []kube.PodSummary{
			{
				Name: "worker-0",
				Labels: map[string]string{
					"okdev.io/workload-role": "Worker",
					"okdev.io/attachable":    "false",
				},
			},
			{
				Name: "master-0",
				Labels: map[string]string{
					"okdev.io/workload-role": "Master",
					"okdev.io/attachable":    "true",
				},
			},
		},
	}

	if _, _, err := resolveTargetCandidate(view, "worker-0", ""); err == nil {
		t.Fatal("expected non-attachable pod selection to fail")
	}
}

func TestResolveTargetCandidateAllowsPodSelectionWithoutAttachableLabels(t *testing.T) {
	view := sessionView{
		Session: "sess1",
		Pods: []kube.PodSummary{
			{Name: "okdev-sess1"},
		},
	}

	got, role, err := resolveTargetCandidate(view, "okdev-sess1", "")
	if err != nil {
		t.Fatalf("resolveTargetCandidate: %v", err)
	}
	if got.Name != "okdev-sess1" {
		t.Fatalf("expected okdev-sess1, got %q", got.Name)
	}
	if role != "" {
		t.Fatalf("expected empty role, got %q", role)
	}
}

func TestResolveTargetCandidateErrors(t *testing.T) {
	view := sessionView{
		Session: "sess1",
		Pods: []kube.PodSummary{
			{
				Name: "master-0",
				Labels: map[string]string{
					"okdev.io/workload-role": "Master",
					"okdev.io/attachable":    "true",
				},
			},
		},
	}

	if _, _, err := resolveTargetCandidate(view, "", ""); err == nil || !strings.Contains(err.Error(), "empty role") {
		t.Fatalf("expected empty role error, got %v", err)
	}
	if _, _, err := resolveTargetCandidate(view, "missing-pod", ""); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("expected pod not found error, got %v", err)
	}
	if _, _, err := resolveTargetCandidate(view, "", "worker"); err == nil || !strings.Contains(err.Error(), "no pods found") {
		t.Fatalf("expected role not found error, got %v", err)
	}
}

func TestSortedSessionPods(t *testing.T) {
	now := time.Now()
	pods := []kube.PodSummary{
		{Name: "old-pending", Phase: "Pending", CreatedAt: now.Add(-time.Minute)},
		{Name: "new-running", Phase: "Running", Ready: "1/1", CreatedAt: now},
	}

	got := sortedSessionPods(pods)
	if len(got) != 2 {
		t.Fatalf("unexpected pod count %d", len(got))
	}
	if got[0].Name != "new-running" {
		t.Fatalf("expected running pod first, got %q", got[0].Name)
	}
	if pods[0].Name != "old-pending" {
		t.Fatal("expected original slice to remain unchanged")
	}
}
