package workload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
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
			"okdev.io/repo":          "okdev",
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
