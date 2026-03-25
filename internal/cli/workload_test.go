package cli

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	corev1 "k8s.io/api/core/v1"
)

func TestSessionRuntimeReturnsGenericRuntimeForPyTorchJob(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Type = workload.TypePyTorchJob
	cfg.Spec.Workload.ManifestPath = ".okdev/pytorchjob.yaml"
	cfg.Spec.Workload.Inject = []config.WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Master.template"}}

	rt, err := sessionRuntime(cfg, "/tmp/.okdev.yaml", "sess1", nil, nil, corev1.PodSpec{}, nil, false, "")
	if err != nil {
		t.Fatalf("sessionRuntime: %v", err)
	}
	gen, ok := rt.(*workload.GenericRuntime)
	if !ok {
		t.Fatalf("expected GenericRuntime, got %T", rt)
	}
	if gen.Kind() != workload.TypePyTorchJob {
		t.Fatalf("unexpected kind: %s", gen.Kind())
	}
}

func TestSessionRuntimeReturnsGenericRuntimeForJob(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Type = workload.TypeJob
	cfg.Spec.Workload.ManifestPath = ".okdev/job.yaml"
	cfg.Spec.Workload.Inject = []config.WorkloadInjectSpec{{Path: "spec.template"}}

	rt, err := sessionRuntime(cfg, "/tmp/.okdev.yaml", "sess1", nil, nil, corev1.PodSpec{}, nil, false, "")
	if err != nil {
		t.Fatalf("sessionRuntime: %v", err)
	}
	gen, ok := rt.(*workload.GenericRuntime)
	if !ok {
		t.Fatalf("expected GenericRuntime, got %T", rt)
	}
	if gen.Kind() != workload.TypeJob {
		t.Fatalf("unexpected kind: %s", gen.Kind())
	}
}

func TestSessionRuntimeReturnsPodRuntimeByDefault(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Sidecar.Image = "ghcr.io/acmore/okdev:edge"

	rt, err := sessionRuntime(cfg, "/tmp/.okdev.yaml", "sess1", nil, nil, corev1.PodSpec{}, nil, false, "")
	if err != nil {
		t.Fatalf("sessionRuntime: %v", err)
	}
	_, ok := rt.(*workload.PodRuntime)
	if !ok {
		t.Fatalf("expected PodRuntime, got %T", rt)
	}
	if rt.Kind() != workload.TypePod {
		t.Fatalf("unexpected kind: %s", rt.Kind())
	}
}

type fakeResolveTargetClient struct {
	kube.PodSummary
	listSel string
}

func (f *fakeResolveTargetClient) GetPodSummary(_ context.Context, namespace, name string) (*kube.PodSummary, error) {
	if name != f.Name {
		return nil, errors.New("not found")
	}
	return &kube.PodSummary{Name: name, Namespace: namespace}, nil
}

func (f *fakeResolveTargetClient) ListPods(_ context.Context, namespace string, _ bool, labelSelector string) ([]kube.PodSummary, error) {
	f.listSel = labelSelector
	return []kube.PodSummary{{Name: f.Name, Namespace: namespace, Labels: f.Labels}}, nil
}

func TestResolveTargetRefRecoversControllerBackedTargetWhenLocalStateIsMissing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "trainer"
	cfg.Spec.Workload.Type = workload.TypeJob
	cfg.Spec.Workload.ManifestPath = ".okdev/job.yaml"
	cfg.Spec.Workload.Attach.Container = "trainer"
	cfg.Spec.Sidecar.Image = "ghcr.io/acmore/okdev:edge"
	cfg.Spec.Workload.Inject = []config.WorkloadInjectSpec{{Path: "spec.template"}}
	cfg.Spec.Session.DefaultNameTemplate = "{{ .Repo }}"

	client := &fakeResolveTargetClient{
		PodSummary: kube.PodSummary{
			Name: "trainer-0",
			Labels: map[string]string{
				"okdev.io/attachable": "true",
			},
		},
	}
	cfgPath := filepath.Join(t.TempDir(), ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte("kind: DevEnvironment\n"), 0o644); err != nil {
		t.Fatalf("write config path: %v", err)
	}
	target, err := resolveTargetRef(context.Background(), &Options{ConfigPath: cfgPath}, cfg, "default", "sess1", client)
	if err != nil {
		t.Fatalf("resolveTargetRef: %v", err)
	}
	if target.PodName != "trainer-0" || target.Container != "trainer" {
		t.Fatalf("unexpected target: %+v", target)
	}
	expectedSelector := "okdev.io/managed=true,okdev.io/name=trainer,okdev.io/session=sess1,okdev.io/workload-type=job"
	if client.listSel != expectedSelector {
		t.Fatalf("expected selector %q, got %q", expectedSelector, client.listSel)
	}
}
