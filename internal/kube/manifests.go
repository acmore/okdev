package kube

import (
	"fmt"

	"github.com/acmore/okdev/internal/config"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"sigs.k8s.io/yaml"
)

func BuildPVCManifest(namespace, name, size, storageClass string, labels map[string]string, annotations map[string]string) ([]byte, error) {
	if size == "" {
		size = config.DefaultWorkspacePVCSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parse pvc size %q: %w", size, err)
	}
	pvcSpec := corev1.PersistentVolumeClaimSpec{
		AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: qty,
			},
		},
	}
	if storageClass != "" {
		pvcSpec.StorageClassName = &storageClass
	}
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": pvcSpec,
	}
	b, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal pvc manifest: %w", err)
	}
	return b, nil
}

func BuildPodManifest(namespace, name string, labels map[string]string, annotations map[string]string, podSpec corev1.PodSpec) ([]byte, error) {
	manifest := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":        name,
			"namespace":   namespace,
			"labels":      labels,
			"annotations": annotations,
		},
		"spec": podSpec,
	}

	b, err := yaml.Marshal(manifest)
	if err != nil {
		return nil, fmt.Errorf("marshal pod manifest: %w", err)
	}
	return b, nil
}
