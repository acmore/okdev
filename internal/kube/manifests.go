package kube

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

func BuildPVCManifest(namespace, name, size, storageClass string, labels map[string]string) ([]byte, error) {
	if size == "" {
		size = "50Gi"
	}
	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "PersistentVolumeClaim",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
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

func BuildPodManifest(namespace, name, claimName string, labels map[string]string, podSpec map[string]any) ([]byte, error) {
	spec := podSpec
	if len(spec) == 0 {
		spec = map[string]any{
			"containers": []map[string]any{
				{
					"name":    "dev",
					"image":   "ubuntu:22.04",
					"command": []string{"sleep", "infinity"},
					"volumeMounts": []map[string]any{
						{"name": "workspace", "mountPath": "/workspace"},
					},
				},
			},
			"volumes": []map[string]any{
				{
					"name": "workspace",
					"persistentVolumeClaim": map[string]any{
						"claimName": claimName,
					},
				},
			},
		}
	}

	m := map[string]any{
		"apiVersion": "v1",
		"kind":       "Pod",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
			"labels":    labels,
		},
		"spec": spec,
	}

	b, err := yaml.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal pod manifest: %w", err)
	}
	return b, nil
}
