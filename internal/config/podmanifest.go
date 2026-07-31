package config

import (
	"errors"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"
)

// SynthesizePodManifest renders a removed spec.podTemplate as a standalone Pod
// manifest. A Pod is a PodTemplateSpec plus apiVersion, kind and a name, so the
// conversion is lossless — which is what makes `okdev migrate` able to promise
// the resulting Pod is unchanged.
//
// Its only caller is that migration. Nothing produces a podTemplate any more.
func SynthesizePodManifest(cfg *DevEnvironment, workloadName string) ([]byte, error) {
	name := strings.TrimSpace(workloadName)
	if name == "" {
		return nil, errors.New("synthesize pod manifest: workload name is required")
	}
	template := PodTemplateRef{}
	if cfg != nil && cfg.Spec.PodTemplate != nil {
		template = *cfg.Spec.PodTemplate
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
