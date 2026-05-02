package workload

import (
	"context"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type captureApplyClient struct {
	manifest []byte
}

func (c *captureApplyClient) Apply(_ context.Context, _ string, manifest []byte) error {
	c.manifest = manifest
	return nil
}

func TestPodRuntimeApplySetsLastAppliedAnnotations(t *testing.T) {
	rt := NewPodRuntime("sess", nil, nil,
		corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}}},
		nil, "/workspace", "sidecar:1", corev1.ResourceRequirements{}, false, "", "", "dev",
	)
	rt.LastAppliedSpecJSON = `{"version":"v1"}`
	rt.LastAppliedSpecHash = "abc123"

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
	if v, ok := annotations[AnnotationLastAppliedHash]; !ok || v != "abc123" {
		t.Fatalf("missing or wrong last-applied-spec-sha256 annotation: %v", annotations)
	}
}

func TestPodRuntimeApplyOmitsAnnotationsWhenEmpty(t *testing.T) {
	rt := NewPodRuntime("sess", nil, nil,
		corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}}},
		nil, "/workspace", "sidecar:1", corev1.ResourceRequirements{}, false, "", "", "dev",
	)

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
	if _, ok := annotations[AnnotationLastAppliedSpec]; ok {
		t.Fatal("expected no last-applied-spec annotation when fields are empty")
	}
}
