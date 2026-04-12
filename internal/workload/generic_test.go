package workload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestGenericRuntimeApplyAndSelectTarget(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "deployment.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trainer
spec:
  selector:
    matchLabels:
      app: trainer
  template:
    metadata:
      labels:
        app: trainer
    spec:
      containers:
        - name: trainer
          image: python:3.12
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &GenericRuntime{
		ManifestPath:       manifestPath,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		SidecarResources:   corev1.ResourceRequirements{},
		TargetContainer:    "trainer",
		Volumes: []corev1.Volume{{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		Labels: map[string]string{
			"okdev.io/managed": "true",
			"okdev.io/session": "sess1",
		},
		Inject: []config.WorkloadInjectSpec{{Path: "spec.template"}},
	}
	client := &fakeGenericClient{
		pods: []kube.PodSummary{
			{
				Name:      "trainer-abc",
				Phase:     "Running",
				Ready:     "1/1",
				CreatedAt: time.Now(),
				Labels:    map[string]string{"okdev.io/attachable": "true"},
			},
		},
	}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(client.manifest) == 0 {
		t.Fatal("expected generic manifest to be applied")
	}
	target, err := rt.SelectTarget(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.PodName != "trainer-abc" || target.Container != "trainer" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if err := rt.Delete(context.Background(), client, "default", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if client.fakeDeleteClient.kind != "Deployment" || client.fakeDeleteClient.name != "trainer" {
		t.Fatalf("unexpected delete call: %+v", client.fakeDeleteClient)
	}
}

func TestGenericRuntimeUsesSessionNameForWorkload(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "deployment.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: original-name
  labels:
    team: ml
spec:
  selector:
    matchLabels:
      app: trainer
  template:
    metadata:
      labels:
        app: trainer
    spec:
      containers:
        - name: trainer
          image: python:3.12
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &GenericRuntime{
		SessionName:        "my-session",
		ManifestPath:       manifestPath,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		SidecarResources:   corev1.ResourceRequirements{},
		TargetContainer:    "trainer",
		Volumes: []corev1.Volume{{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		Labels: map[string]string{
			"okdev.io/managed": "true",
			"okdev.io/session": "my-session",
		},
		Inject: []config.WorkloadInjectSpec{{Path: "spec.template"}},
	}

	if got := rt.WorkloadName(); got != "okdev-my-session" {
		t.Fatalf("expected workload name to be session-derived, got %q", got)
	}

	_, kind, name, err := rt.WorkloadRef()
	if err != nil {
		t.Fatalf("WorkloadRef: %v", err)
	}
	if kind != "Deployment" || name != "okdev-my-session" {
		t.Fatalf("unexpected workload ref: kind=%q name=%q", kind, name)
	}

	client := &fakeGenericClient{}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(client.manifest, &obj); err != nil {
		t.Fatal(err)
	}
	meta, _ := obj["metadata"].(map[string]any)
	if meta["name"] != "okdev-my-session" {
		t.Fatalf("expected applied manifest name to be session-derived, got %v", meta["name"])
	}
	labels, _ := meta["labels"].(map[string]any)
	if labels["team"] != "ml" {
		t.Fatalf("expected user labels to be preserved, got %v", labels)
	}

	if err := rt.Delete(context.Background(), client, "default", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if client.fakeDeleteClient.name != "okdev-my-session" {
		t.Fatalf("expected delete to use session name, got %q", client.fakeDeleteClient.name)
	}
}

func TestGenericRuntimeFallsBackToManifestName(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "deployment.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: fallback-name
spec:
  template:
    spec:
      containers:
        - name: trainer
          image: python:3.12
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &GenericRuntime{ManifestPath: manifestPath}
	if got := rt.WorkloadName(); got != "fallback-name" {
		t.Fatalf("expected fallback to manifest name, got %q", got)
	}
}

func TestGenericRuntimeSelectTargetPrefersAttachablePods(t *testing.T) {
	rt := &GenericRuntime{
		WorkloadKind:    TypeGeneric,
		TargetContainer: "trainer",
		Labels: map[string]string{
			"okdev.io/managed": "true",
			"okdev.io/session": "sess1",
		},
	}
	client := &fakeGenericClient{
		pods: []kube.PodSummary{
			{
				Name:   "worker-0",
				Phase:  "Running",
				Ready:  "1/1",
				Labels: map[string]string{"okdev.io/attachable": "false", "okdev.io/workload-role": "Worker"},
			},
			{
				Name:   "master-0",
				Phase:  "Running",
				Ready:  "1/1",
				Labels: map[string]string{"okdev.io/attachable": "true", "okdev.io/workload-role": "Master"},
			},
		},
	}
	target, err := rt.SelectTarget(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.PodName != "master-0" || target.Role != "Master" {
		t.Fatalf("unexpected target: %+v", target)
	}
}

func TestGenericRuntimeSelectTargetFailsWithoutAttachablePods(t *testing.T) {
	rt := &GenericRuntime{
		WorkloadKind:    TypeGeneric,
		TargetContainer: "trainer",
		Labels: map[string]string{
			"okdev.io/managed":       "true",
			"okdev.io/session":       "sess1",
			"okdev.io/name":          "trainer",
			"okdev.io/workload-type": "generic",
		},
	}
	client := &fakeGenericClient{
		pods: []kube.PodSummary{
			{
				Name:   "worker-0",
				Phase:  "Running",
				Ready:  "1/1",
				Labels: map[string]string{"okdev.io/attachable": "false", "okdev.io/workload-role": "Worker"},
			},
		},
	}
	if _, err := rt.SelectTarget(context.Background(), client, "default"); err == nil {
		t.Fatal("expected SelectTarget to fail without attachable pods")
	}
}

func TestGenericRuntimeLoadInvalidatesManifestCacheOnFileChange(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "deployment.yaml")
	initial := []byte(`
apiVersion: apps/v1
kind: Deployment
metadata:
  name: trainer
spec:
  template:
    spec:
      containers:
        - name: trainer
          image: python:3.12
`)
	if err := os.WriteFile(manifestPath, initial, 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &GenericRuntime{ManifestPath: manifestPath}
	if got := rt.WorkloadName(); got != "trainer" {
		t.Fatalf("expected initial workload name, got %q", got)
	}
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(manifestPath, []byte(strings.ReplaceAll(string(initial), "trainer", "mutated")), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := rt.WorkloadName(); got != "mutated" {
		t.Fatalf("expected refreshed workload name, got %q", got)
	}
}

func TestGenericRuntimeApplyInjectsPreStopWhenSidecarDisabled(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "pytorchjob.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
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
            - name: pytorch
              image: python:3.12
    Worker:
      template:
        spec:
          containers:
            - name: pytorch
              image: python:3.12
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &GenericRuntime{
		WorkloadKind:       TypeGeneric,
		ManifestPath:       manifestPath,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		SidecarResources:   corev1.ResourceRequirements{},
		TargetContainer:    "pytorch",
		PreStop:            "touch /tmp/pre-stop-marker",
		Inject: []config.WorkloadInjectSpec{
			{Path: "spec.pytorchReplicaSpecs.Master.template"},
			{Path: "spec.pytorchReplicaSpecs.Worker.template", Sidecar: boolPtr(false)},
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
	workerTemplate, err := resolveMapPath(obj, "spec.pytorchReplicaSpecs.Worker.template")
	if err != nil {
		t.Fatal(err)
	}
	template, err := decodePodTemplateSpec(workerTemplate)
	if err != nil {
		t.Fatal(err)
	}
	worker := template.Spec.Containers[0]
	if worker.Lifecycle == nil || worker.Lifecycle.PreStop == nil {
		t.Fatal("expected prestop lifecycle on worker container")
	}
	if got := strings.Join(worker.Lifecycle.PreStop.Exec.Command, " "); !strings.Contains(got, "pre-stop-marker") {
		t.Fatalf("expected prestop command to include marker, got %q", got)
	}
	for _, c := range template.Spec.Containers {
		if c.Name == "okdev-sidecar" {
			t.Fatal("did not expect sidecar injection when sidecar=false")
		}
	}
}

func boolPtr(v bool) *bool {
	return &v
}

func TestPodTemplateSpecUnstructuredRoundTrip(t *testing.T) {
	src := map[string]any{
		"metadata": map[string]any{
			"labels": map[string]any{
				"app": "trainer",
			},
		},
		"spec": map[string]any{
			"containers": []any{
				map[string]any{
					"name":  "trainer",
					"image": "python:3.12",
					"env": []any{
						map[string]any{
							"name":  "MODE",
							"value": "train",
						},
					},
				},
			},
		},
	}

	template, err := decodePodTemplateSpec(src)
	if err != nil {
		t.Fatalf("decodePodTemplateSpec: %v", err)
	}
	if template.Labels["app"] != "trainer" {
		t.Fatalf("unexpected labels: %+v", template.Labels)
	}
	if len(template.Spec.Containers) != 1 || template.Spec.Containers[0].Image != "python:3.12" {
		t.Fatalf("unexpected containers: %+v", template.Spec.Containers)
	}

	out, err := encodePodTemplateSpec(template)
	if err != nil {
		t.Fatalf("encodePodTemplateSpec: %v", err)
	}
	spec, ok := out["spec"].(map[string]any)
	if !ok {
		t.Fatalf("expected spec map, got %#v", out["spec"])
	}
	containers, ok := spec["containers"].([]any)
	if !ok || len(containers) != 1 {
		t.Fatalf("unexpected containers payload: %#v", spec["containers"])
	}
	first, ok := containers[0].(map[string]any)
	if !ok {
		t.Fatalf("expected first container map, got %#v", containers[0])
	}
	if first["image"] != "python:3.12" {
		t.Fatalf("unexpected image in round-trip: %#v", first["image"])
	}
}
