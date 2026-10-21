package cli

import (
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// okdevInternalVolumes back okdev's own machinery (syncthing state, runtime
// files), not places for the user's data, so the storage summary skips them.
var okdevInternalVolumes = map[string]bool{"okdev-runtime": true, "syncthing-home": true}

// upStorageLines classifies the dev container's mounts so long runs can put
// artifacts on storage that survives the pod (#344).
func upStorageLines(pod *corev1.Pod, targetContainer string) []string {
	if pod == nil {
		return nil
	}
	var lines []string
	for _, mount := range podMountsForContainer(pod, targetContainer) {
		if okdevInternalVolumes[mount.Name] {
			continue
		}
		var label string
		switch mount.SourceType {
		case "emptyDir":
			label = "node-local emptyDir, lost when this pod is replaced"
		case "hostPath":
			label = "node-local hostPath, may be unavailable on another node"
		case "persistentVolumeClaim":
			label = "PVC-backed"
		default:
			continue
		}
		lines = append(lines, fmt.Sprintf("%s (%s)", mount.MountPath, label))
	}
	return lines
}
