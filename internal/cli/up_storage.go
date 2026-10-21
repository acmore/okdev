package cli

import (
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"
)

func printUpStorageNotice(w io.Writer, pod *corev1.Pod, targetContainer string) {
	if pod == nil {
		return
	}
	// Only the dev container: the sidecar's own mounts (syncthing home,
	// okdev runtime) are okdev internals, not places for the user's data.
	printed := false
	for _, container := range []string{targetContainer} {
		for _, mount := range podMountsForContainer(pod, container) {
			var label string
			switch mount.SourceType {
			case "emptyDir":
				label = "node-local emptyDir (lost when this pod is replaced)"
			case "hostPath":
				label = "node-local hostPath (may be unavailable on another node)"
			case "persistentVolumeClaim":
				label = "PVC-backed"
			default:
				continue
			}
			if !printed {
				fmt.Fprintln(w, "storage mounts:")
				printed = true
			}
			fmt.Fprintf(w, "- %s:%s — %s\n", container, mount.MountPath, label)
		}
	}
}
