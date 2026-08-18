package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
)

func meshPod(name string, eligible bool, sidecar bool) kube.PodSummary {
	p := kube.PodSummary{
		Name:   name,
		Phase:  "Running",
		Labels: map[string]string{workload.MeshEligibleLabel: "false"},
	}
	if eligible {
		p.Labels[workload.MeshEligibleLabel] = "true"
	}
	if sidecar {
		p.ContainerImages = []kube.ContainerImage{{Name: "okdev-sidecar", Image: "ghcr.io/acmore/okdev:edge"}}
	}
	return p
}

// Printing nothing when there was no mesh made "no mesh needed" and "mesh
// silently never ran" indistinguishable, which is how a workload whose workers
// sat on an empty workspace went unreported. Every shape must say something.
func TestMeshLinesAlwaysStateTheDecision(t *testing.T) {
	cases := []struct {
		name string
		pods []kube.PodSummary
		hub  string
		want string
	}{
		{
			name: "receivers get the hub-and-spoke topology",
			pods: []kube.PodSummary{meshPod("master-0", true, true), meshPod("worker-0", true, true)},
			hub:  "master-0",
			want: "topology: hub-and-spoke",
		},
		{
			name: "a lone pod has nothing to distribute",
			pods: []kube.PodSummary{meshPod("master-0", true, true)},
			hub:  "master-0",
			want: "single pod",
		},
		{
			name: "a shared volume needs no mesh",
			pods: []kube.PodSummary{meshPod("master-0", true, true), meshPod("worker-0", false, true)},
			hub:  "master-0",
			want: "shared volume",
		},
		{
			name: "a pod with no sidecar receives nothing",
			pods: []kube.PodSummary{meshPod("master-0", true, true), meshPod("worker-0", false, false)},
			hub:  "master-0",
			want: "no sidecar",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			lines := buildMeshLinesFromPods(tc.pods, tc.hub)
			if len(lines) == 0 {
				t.Fatal("status must never be silent about how the workspace is distributed")
			}
			if !strings.Contains(strings.Join(lines, "\n"), tc.want) {
				t.Fatalf("lines = %v, want one containing %q", lines, tc.want)
			}
		})
	}
}

// The hub is where the workspace already is, so it is never listed as needing
// it sent to them.
func TestMeshLinesNeverListTheHubAsAReceiver(t *testing.T) {
	lines := buildMeshLinesFromPods(
		[]kube.PodSummary{meshPod("master-0", true, true), meshPod("worker-0", true, true)},
		"master-0",
	)
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "receivers: 1") {
		t.Fatalf("expected exactly one receiver, got:\n%s", joined)
	}
	if !strings.Contains(joined, "hub: master-0") {
		t.Fatalf("expected master-0 named as hub, got:\n%s", joined)
	}
}
