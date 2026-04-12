package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
)

func TestDiscoveryLabelsForSessionSkipsEmptyWorkloadType(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "trainer"

	labels := discoveryLabelsForSession(cfg, "sess1", "")
	if _, ok := labels["okdev.io/workload-type"]; ok {
		t.Fatalf("expected empty workload type to be omitted, got %+v", labels)
	}
}

func TestWorkloadNameFromPodSummaryPrefersLabelOverAnnotation(t *testing.T) {
	pod := kube.PodSummary{
		Labels:      map[string]string{"okdev.io/workload-name": "from-label"},
		Annotations: map[string]string{"okdev.io/workload-name": "from-annotation"},
	}
	if got := workloadNameFromPodSummary(pod); got != "from-label" {
		t.Fatalf("expected label value, got %q", got)
	}
}

func TestWorkloadNameFromPodSummaryFallsBackToAnnotation(t *testing.T) {
	pod := kube.PodSummary{
		Labels:      map[string]string{},
		Annotations: map[string]string{"okdev.io/workload-name": "from-annotation"},
	}
	if got := workloadNameFromPodSummary(pod); got != "from-annotation" {
		t.Fatalf("expected annotation value, got %q", got)
	}
}

func TestWorkloadNameFromPodSummaryReturnsEmptyWhenMissing(t *testing.T) {
	pod := kube.PodSummary{
		Labels: map[string]string{},
	}
	if got := workloadNameFromPodSummary(pod); got != "" {
		t.Fatalf("expected empty, got %q", got)
	}
}
