package workload

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"sigs.k8s.io/yaml"
)

func TestGenericRuntimeApplySetsLastAppliedAnnotations(t *testing.T) {
	dir := t.TempDir()
	manifest := filepath.Join(dir, "job.yaml")
	os.WriteFile(manifest, []byte(`apiVersion: batch/v1
kind: Job
metadata:
  name: trainer
spec:
  template:
    spec:
      containers:
      - name: dev
        image: ubuntu:22.04
      restartPolicy: Never
`), 0o644)

	rt := &GenericRuntime{
		WorkloadKind:        "job",
		ManifestPath:        manifest,
		WorkspaceMountPath:  "/workspace",
		SidecarImage:        "sidecar:1",
		SidecarResources:    config.DevEnvironment{}.Spec.Sidecar.Resources,
		TargetContainer:     "dev",
		Inject:              []config.WorkloadInjectSpec{{Path: "spec.template"}},
		LastAppliedSpecJSON: `{"version":"v1"}`,
		LastAppliedSpecHash: "def456",
	}

	client := &captureApplyClient{}
	if err := rt.Apply(context.Background(), client, "default"); err != nil {
		t.Fatal(err)
	}

	var obj map[string]any
	if err := yaml.Unmarshal(client.manifest, &obj); err != nil {
		t.Fatal(err)
	}
	meta := obj["metadata"].(map[string]any)
	annotations := meta["annotations"].(map[string]any)
	if v, ok := annotations[AnnotationLastAppliedSpec]; !ok || !strings.Contains(v.(string), "v1") {
		t.Fatalf("missing or wrong last-applied-spec annotation: %v", annotations)
	}
	if v, ok := annotations[AnnotationLastAppliedHash]; !ok || v != "def456" {
		t.Fatalf("missing or wrong last-applied-spec-sha256 annotation: %v", annotations)
	}
}
