package kube

import "fmt"

func PreparePodSpec(podSpec map[string]any, workspaceClaim, workspaceMountPath string, syncthingEnabled bool, syncthingImage string) (map[string]any, error) {
	spec := podSpec
	if len(spec) == 0 {
		spec = map[string]any{}
	}

	containers, _ := spec["containers"].([]any)
	if len(containers) == 0 {
		containers = []any{map[string]any{
			"name":    "dev",
			"image":   "ubuntu:22.04",
			"command": []any{"sleep", "infinity"},
		}}
	}

	volumes, _ := spec["volumes"].([]any)
	volumes = ensureVolume(volumes, "workspace", map[string]any{
		"persistentVolumeClaim": map[string]any{"claimName": workspaceClaim},
	})

	for i, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		vms, _ := cm["volumeMounts"].([]any)
		vms = ensureVolumeMount(vms, "workspace", workspaceMountPath)
		cm["volumeMounts"] = vms
		containers[i] = cm
	}

	if syncthingEnabled {
		if syncthingImage == "" {
			return nil, fmt.Errorf("syncthing image cannot be empty when syncthing is enabled")
		}
		volumes = ensureVolume(volumes, "syncthing-home", map[string]any{"emptyDir": map[string]any{}})
		if !hasContainer(containers, "syncthing") {
			containers = append(containers, map[string]any{
				"name":    "syncthing",
				"image":   syncthingImage,
				"command": []any{"sh", "-lc", "syncthing -home /var/syncthing -no-browser -gui-address=0.0.0.0:8384 -no-restart -skip-port-probing -listen=tcp://0.0.0.0:22000"},
				"ports": []any{
					map[string]any{"containerPort": 8384, "name": "st-gui"},
					map[string]any{"containerPort": 22000, "name": "st-sync"},
				},
				"volumeMounts": []any{
					map[string]any{"name": "workspace", "mountPath": workspaceMountPath},
					map[string]any{"name": "syncthing-home", "mountPath": "/var/syncthing"},
				},
			})
		}
	}

	spec["containers"] = containers
	spec["volumes"] = volumes
	return spec, nil
}

func ensureVolume(volumes []any, name string, def map[string]any) []any {
	for _, v := range volumes {
		vm, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if vm["name"] == name {
			return volumes
		}
	}
	m := map[string]any{"name": name}
	for k, v := range def {
		m[k] = v
	}
	return append(volumes, m)
}

func ensureVolumeMount(mounts []any, name, mountPath string) []any {
	for _, m := range mounts {
		mm, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if mm["name"] == name {
			if mm["mountPath"] == nil {
				mm["mountPath"] = mountPath
			}
			return mounts
		}
	}
	return append(mounts, map[string]any{"name": name, "mountPath": mountPath})
}

func hasContainer(containers []any, name string) bool {
	for _, c := range containers {
		cm, ok := c.(map[string]any)
		if !ok {
			continue
		}
		if cm["name"] == name {
			return true
		}
	}
	return false
}
