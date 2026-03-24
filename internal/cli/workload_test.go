package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	corev1 "k8s.io/api/core/v1"
)

func TestSessionRuntimeSupportsPyTorchJobViaGenericRuntime(t *testing.T) {
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
