package config

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// SynthesizePodManifest renders spec.podTemplate as a standalone Pod manifest.
//
// It is what lets pod workloads share the manifest-based runtime with job,
// pytorchjob and generic: a Pod is a PodTemplateSpec plus apiVersion, kind and
// a name. The name is baked in rather than left as a "{{ .WorkloadName }}"
// placeholder because synthesized manifests are applied verbatim — a user pod
// spec may legitimately contain "{{".
func SynthesizePodManifest(cfg *DevEnvironment, workloadName string) ([]byte, error) {
	name := strings.TrimSpace(workloadName)
	if name == "" {
		return nil, errors.New("synthesize pod manifest: workload name is required")
	}
	template := PodTemplateRef{}
	if cfg != nil {
		template = cfg.Spec.PodTemplate
	}
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{APIVersion: "v1", Kind: "Pod"},
		ObjectMeta: metav1.ObjectMeta{
			Name:   name,
			Labels: template.Metadata.Labels,
		},
		Spec: template.Spec,
	}
	raw, err := yaml.Marshal(&pod)
	if err != nil {
		return nil, fmt.Errorf("synthesize pod manifest: %w", err)
	}
	return raw, nil
}
