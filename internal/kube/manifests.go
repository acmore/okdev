package kube

import (
	"fmt"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BuildPVCManifest(namespace, name, size, storageClass string, labels map[string]string, annotations map[string]string) ([]byte, error) {
	if size == "" {
		size = "50Gi"
	}
	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": map[string]any{
			"accessModes": []string{"ReadWriteOnce"},
			"resources": map[string]any{
				"requests": map[string]string{
					"storage": size,
				},
			},
		},
	}
	if storageClass != "" {
		m["spec"].(map[string]any)["storageClassName"] = storageClass
	}
	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal pvc manifest: %w", err)
	}
	return b, nil
}

func BuildPodManifest(namespace, name string, labels map[string]string, annotations map[string]string, podSpec corev1.PodSpec) ([]byte, error) {
	pod := corev1.Pod{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "Pod",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: podSpec,
	}

	b, err := yaml.Marshal(pod)
	if err != nil {
		return nil, fmt.Errorf("marshal pod manifest: %w", err)
	}
	return b, nil
}
