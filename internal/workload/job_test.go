package workload

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
)

type fakeJobClient struct {
	fakeApplyClient
	fakeDeleteClient
	fakeWaitClient
	pods []kube.PodSummary
}

func (f *fakeJobClient) GetPodSummary(_ context.Context, namespace, name string) (*kube.PodSummary, error) {
	return &kube.PodSummary{Name: name, Namespace: namespace}, nil
}

func (f *fakeJobClient) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return f.pods, nil
}

func TestJobRuntimeApplyAndSelectTarget(t *testing.T) {
	tmp := t.TempDir()
	manifestPath := filepath.Join(tmp, "job.yaml")
	if err := os.WriteFile(manifestPath, []byte(`
apiVersion: batch/v1
kind: Job
metadata:
  name: trainer
spec:
  template:
    spec:
      restartPolicy: Never
      containers:
        - name: trainer
          image: python:3.12
`), 0o644); err != nil {
		t.Fatal(err)
	}

	rt := &JobRuntime{
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
	}

	client := &fakeJobClient{
		pods: []kube.PodSummary{
			{Name: "trainer-older", Phase: "Running", Ready: "0/1", CreatedAt: time.Now().Add(-2 * time.Minute)},
			{Name: "trainer-newer", Phase: "Running", Ready: "1/1", CreatedAt: time.Now().Add(-1 * time.Minute)},
		},
	}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if len(client.manifest) == 0 {
		t.Fatal("expected job manifest to be applied")
	}
	target, err := rt.SelectTarget(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.PodName != "trainer-newer" || target.Container != "trainer" {
		t.Fatalf("unexpected target: %+v", target)
	}
	if err := rt.WaitReady(context.Background(), client, "default", 30*time.Second, nil); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if client.fakeWaitClient.pod != "trainer-newer" {
		t.Fatalf("unexpected wait pod: %+v", client.fakeWaitClient)
	}
	if err := rt.Delete(context.Background(), client, "default", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if client.fakeDeleteClient.kind != "job" || client.fakeDeleteClient.name != "trainer" {
		t.Fatalf("unexpected delete call: %+v", client.fakeDeleteClient)
	}
}
