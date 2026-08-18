package workload

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// multiPodManifest is a PyTorchJob shaped the way the shipped template ships
// it: two replica specs, no volumes declared, so okdev gives each pod its own
// emptyDir workspace.
const multiPodManifest = `
apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: trainer
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: dev
              image: python:3.12
    Worker:
      template:
        spec:
          containers:
            - name: dev
              image: python:3.12
`

func writeMultiPodManifest(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pytorchjob.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func applyAndDecode(t *testing.T, rt *GenericRuntime, path string) map[string]any {
	t.Helper()
	client := &captureApplyClient{}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(client.manifest, &obj); err != nil {
		t.Fatal(err)
	}
	return obj
}

func templateLabelsAt(t *testing.T, obj map[string]any, path string) map[string]string {
	t.Helper()
	raw, err := resolveMapPath(obj, path)
	if err != nil {
		t.Fatalf("resolve %s: %v", path, err)
	}
	template, err := decodePodTemplateSpec(raw)
	if err != nil {
		t.Fatalf("decode %s: %v", path, err)
	}
	return template.Labels
}

// The shipped default: both replica specs injected, nothing overridden, no
// shared volume. Each pod gets its own emptyDir workspace, so the worker can
// only get the code over the network — it has to be a mesh receiver.
//
// It was labelled a hub instead, because the role was read off `attachable`,
// which defaults to true. Every pod became a hub, no pod was a receiver, mesh
// never engaged, and the worker sat on an empty workspace with a sidecar
// running and nothing to say about it.
func TestDefaultMultiPodWorkloadMakesNonTargetPodsMeshReceivers(t *testing.T) {
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
	obj := applyAndDecode(t, rt, path)

	for _, tc := range []struct{ name, path string }{
		{"master", "spec.pytorchReplicaSpecs.Master.template"},
		{"worker", "spec.pytorchReplicaSpecs.Worker.template"},
	} {
		labels := templateLabelsAt(t, obj, tc.path)
		if got := labels[MeshEligibleLabel]; got != "true" {
			t.Errorf("%s: %s = %q, want \"true\" — a private workspace can only be filled over the mesh",
				tc.name, MeshEligibleLabel, got)
		}
	}
}

// A workspace backed by a shared claim needs no mesh: the volume already
// carries the code to every pod that mounts it.
func TestSharedWorkspaceVolumeIsNotMeshEligible(t *testing.T) {
	shared := `
apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: trainer
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          volumes:
            - name: workspace
              persistentVolumeClaim:
                claimName: shared-ws
          containers:
            - name: dev
              image: python:3.12
    Worker:
      template:
        spec:
          volumes:
            - name: workspace
              persistentVolumeClaim:
                claimName: shared-ws
          containers:
            - name: dev
              image: python:3.12
`
	path := writeMultiPodManifest(t, shared)
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
	obj := applyAndDecode(t, rt, path)

	labels := templateLabelsAt(t, obj, "spec.pytorchReplicaSpecs.Worker.template")
	if got := labels[MeshEligibleLabel]; got == "true" {
		t.Errorf("a PVC-backed workspace must not be mesh-eligible, got %s=%q", MeshEligibleLabel, got)
	}
}

// No sidecar means no syncthing, so the pod cannot receive anything however
// its workspace is backed.
func TestPodWithoutSidecarIsNotMeshEligible(t *testing.T) {
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
			{Path: "spec.pytorchReplicaSpecs.Worker.template", Sidecar: boolPtr(false)},
		},
	}
	obj := applyAndDecode(t, rt, path)

	labels := templateLabelsAt(t, obj, "spec.pytorchReplicaSpecs.Worker.template")
	if got := labels[MeshEligibleLabel]; got == "true" {
		t.Errorf("a pod with no sidecar must not be mesh-eligible, got %s=%q", MeshEligibleLabel, got)
	}
}

// attachable decides where a bare `okdev ssh` lands. It must not decide
// anything about how the workspace gets there — conflating the two is what
// made a worker you could attach to a worker that never received the code.
func TestAttachableDoesNotDecideMeshEligibility(t *testing.T) {
	path := writeMultiPodManifest(t, multiPodManifest)
	attachable := true
	rt := &GenericRuntime{
		WorkloadKind:       TypeGeneric,
		ManifestPath:       path,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		TargetContainer:    "dev",
		Labels:             map[string]string{"okdev.io/session": "sess"},
		Inject: []config.WorkloadInjectSpec{
			{Path: "spec.pytorchReplicaSpecs.Master.template"},
			{Path: "spec.pytorchReplicaSpecs.Worker.template", Attachable: &attachable},
		},
	}
	obj := applyAndDecode(t, rt, path)

	labels := templateLabelsAt(t, obj, "spec.pytorchReplicaSpecs.Worker.template")
	if got := labels["okdev.io/attachable"]; got != "true" {
		t.Errorf("attachable must still control interactive targeting, got %q", got)
	}
	if got := labels[MeshEligibleLabel]; got != "true" {
		t.Errorf("an attachable pod with a private workspace is still a receiver, got %s=%q", MeshEligibleLabel, got)
	}
}

var _ = corev1.PodSpec{}
