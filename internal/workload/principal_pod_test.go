package workload

import (
	"context"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"sigs.k8s.io/yaml"
)

// The config declares inject paths in order, and the first one is the shape you
// work in — Master before Worker, in every template that ships. Pods carry that
// rank so the choice of target does not come down to creation time.
func TestInjectOrderIsStampedOnPods(t *testing.T) {
	path := writeMultiPodManifest(t, multiPodManifest)
	rt := &GenericRuntime{
		WorkloadKind:       TypeGeneric,
		ManifestPath:       path,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		TargetContainer:    "dev",
		Labels:             map[string]string{"okdev.io/session": "sess"},
		Inject: []config.WorkloadInjectSpec{
			{Path: "spec.pytorchReplicaSpecs.Master.template"},
			{Path: "spec.pytorchReplicaSpecs.Worker.template"},
		},
	}
	client := &captureApplyClient{}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(client.manifest, &obj); err != nil {
		t.Fatal(err)
	}
	for path, want := range map[string]string{
		"spec.pytorchReplicaSpecs.Master.template": "0",
		"spec.pytorchReplicaSpecs.Worker.template": "1",
	} {
		if got := templateLabelsAt(t, obj, path)[WorkloadRankLabel]; got != want {
			t.Errorf("%s: %s = %q, want %q", path, WorkloadRankLabel, got, want)
		}
	}
}

// A wandering target is not a cosmetic problem: the target is also the sync
// hub, so letting it follow creation time meant an `okdev up` could re-bootstrap
// the whole workspace from a different pod. The first declared shape wins.
func TestPrincipalPodOutranksLaterReplicas(t *testing.T) {
	older := time.Now().Add(-10 * time.Minute)
	newer := time.Now()

	master := kube.PodSummary{
		Name: "master-0", Phase: "Running", Ready: "2/2", CreatedAt: older,
		Labels: map[string]string{WorkloadRankLabel: "0"},
	}
	worker := kube.PodSummary{
		Name: "worker-0", Phase: "Running", Ready: "2/2", CreatedAt: newer,
		Labels: map[string]string{WorkloadRankLabel: "1"},
	}

	if !ComparePodPriority(master, worker) {
		t.Fatal("the first declared shape must outrank a later replica, even when the replica is newer")
	}
	if ComparePodPriority(worker, master) {
		t.Fatal("ordering must be antisymmetric")
	}
}

// Rank must not override an explicit attach: a pod someone actually attached to
// stays the target, which is what makes `okdev use` and repinning stick.
func TestLastAttachStillWinsOverRank(t *testing.T) {
	master := kube.PodSummary{
		Name: "master-0", Phase: "Running", Ready: "2/2",
		Labels: map[string]string{WorkloadRankLabel: "0"},
	}
	worker := kube.PodSummary{
		Name: "worker-0", Phase: "Running", Ready: "2/2",
		Labels:      map[string]string{WorkloadRankLabel: "1"},
		Annotations: map[string]string{"okdev.io/last-attach": time.Now().Format(time.RFC3339)},
	}
	if !ComparePodPriority(worker, master) {
		t.Fatal("an explicitly attached pod must still win")
	}
}

// Pods from before ranking carry no label. Treating a missing rank as principal
// keeps existing sessions resolving to the pod they already used.
func TestMissingRankIsTreatedAsPrincipal(t *testing.T) {
	unlabelled := kube.PodSummary{Name: "a", Phase: "Running", Ready: "2/2"}
	ranked := kube.PodSummary{
		Name: "b", Phase: "Running", Ready: "2/2",
		Labels: map[string]string{WorkloadRankLabel: "3"},
	}
	if !ComparePodPriority(unlabelled, ranked) {
		t.Fatal("a pod with no rank must not be demoted below a ranked replica")
	}
}
