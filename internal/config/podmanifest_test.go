package config

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func TestSynthesizePodManifest(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.PodTemplate = &PodTemplateRef{
		Metadata: MetadataMap{Labels: map[string]string{"team": "ml"}},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name:    "dev",
				Image:   "alpine",
				Command: []string{"sleep", "infinity"},
			}},
		},
	}

	raw, err := SynthesizePodManifest(cfg, "okdev-sess1")
	if err != nil {
		t.Fatalf("SynthesizePodManifest: %v", err)
	}

	var pod corev1.Pod
	if err := yaml.Unmarshal(raw, &pod); err != nil {
		t.Fatalf("unmarshal synthesized manifest: %v\n%s", err, raw)
	}
	if pod.APIVersion != "v1" || pod.Kind != "Pod" {
		t.Fatalf("type meta = %q/%q, want v1/Pod", pod.APIVersion, pod.Kind)
	}
	if pod.Name != "okdev-sess1" {
		t.Fatalf("name = %q, want okdev-sess1", pod.Name)
	}
	if pod.Labels["team"] != "ml" {
		t.Fatalf("labels = %v, want team=ml", pod.Labels)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "alpine" {
		t.Fatalf("spec did not round-trip: %+v", pod.Spec)
	}
}

func TestSynthesizePodManifestKeepsBracesVerbatim(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.PodTemplate = &PodTemplateRef{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{{
				Name: "dev",
				Env:  []corev1.EnvVar{{Name: "TPL", Value: "{{ .Whatever }}"}},
			}},
		},
	}
	raw, err := SynthesizePodManifest(cfg, "okdev-sess1")
	if err != nil {
		t.Fatalf("SynthesizePodManifest: %v", err)
	}
	if !strings.Contains(string(raw), "{{ .Whatever }}") {
		t.Fatalf("braces were mangled:\n%s", raw)
	}
}

func TestSynthesizePodManifestRequiresName(t *testing.T) {
	if _, err := SynthesizePodManifest(&DevEnvironment{}, "  "); err == nil {
		t.Fatal("expected an error for an empty workload name")
	}
}
