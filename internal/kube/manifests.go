package kube

import (
	"fmt"

	"github.com/acmore/okdev/internal/config"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func BuildPVCManifest(namespace, name, size, storageClass string, labels map[string]string, annotations map[string]string) ([]byte, error) {
	if size == "" {
		size = config.DefaultWorkspacePVCSize
	}
	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parse pvc size %q: %w", size, err)
	}
	pvc := corev1.PersistentVolumeClaim{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "v1",
			Kind:       "PersistentVolumeClaim",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        name,
			Namespace:   namespace,
			Labels:      labels,
			Annotations: annotations,
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: qty,
				},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = &storageClass
	}
	b, err := yaml.Marshal(pvc)
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
