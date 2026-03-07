package kube

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var semverTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$`)

func PreparePodSpec(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath string, syncthingEnabled bool, syncthingImage string) (corev1.PodSpec, error) {
	spec := podSpec.DeepCopy()
	if spec == nil {
		spec = &corev1.PodSpec{}
	}
	if len(spec.Containers) == 0 {
		spec.Containers = []corev1.Container{{
			Name:    "dev",
			Image:   "ubuntu:22.04",
			Command: []string{"sleep", "infinity"},
		}}
	}

	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspaceClaim},
		},
	})

	for i := range spec.Containers {
		spec.Containers[i].VolumeMounts = ensureVolumeMount(spec.Containers[i].VolumeMounts, corev1.VolumeMount{
			Name:      "workspace",
			MountPath: workspaceMountPath,
		})
	}

	if syncthingEnabled {
		if syncthingImage == "" {
			return corev1.PodSpec{}, fmt.Errorf("syncthing image cannot be empty when syncthing is enabled")
		}
		spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
			Name:         "syncthing-home",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		})
		if !hasContainer(spec.Containers, "syncthing") {
			spec.Containers = append(spec.Containers, corev1.Container{
				Name:            "syncthing",
				Image:           syncthingImage,
				ImagePullPolicy: syncthingImagePullPolicy(syncthingImage),
				Command:         []string{"sh", "-lc", "syncthing serve --home /var/syncthing --no-browser --gui-address=http://0.0.0.0:8384 --no-restart --skip-port-probing"},
				Ports: []corev1.ContainerPort{
					{ContainerPort: 8384, Name: "st-gui"},
					{ContainerPort: 22000, Name: "st-sync"},
				},
				VolumeMounts: []corev1.VolumeMount{
					{Name: "workspace", MountPath: workspaceMountPath},
					{Name: "syncthing-home", MountPath: "/var/syncthing"},
				},
			})
		}
	}

	return *spec, nil
}

func PreparePodSpecWithSSH(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath string, syncthingEnabled bool, syncthingImage string, sshSidecarEnabled bool, sshSidecarImage string) (corev1.PodSpec, error) {
	spec, err := PreparePodSpec(podSpec, workspaceClaim, workspaceMountPath, syncthingEnabled, syncthingImage)
	if err != nil {
		return corev1.PodSpec{}, err
	}
	if !sshSidecarEnabled {
		return spec, nil
	}
	if strings.TrimSpace(sshSidecarImage) == "" {
		return corev1.PodSpec{}, fmt.Errorf("ssh sidecar image cannot be empty when ssh sidecar is enabled")
	}
	if !hasContainer(spec.Containers, "okdev-ssh") {
		spec.Containers = append(spec.Containers, corev1.Container{
			Name:            "okdev-ssh",
			Image:           sshSidecarImage,
			ImagePullPolicy: syncthingImagePullPolicy(sshSidecarImage),
			Ports: []corev1.ContainerPort{
				{ContainerPort: 22, Name: "ssh"},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: workspaceMountPath},
			},
		})
	}
	return spec, nil
}

func syncthingImagePullPolicy(image string) corev1.PullPolicy {
	if strings.Contains(image, "@sha256:") {
		return corev1.PullIfNotPresent
	}
	tag := imageTag(image)
	if tag == "" {
		return corev1.PullAlways
	}
	if semverTagPattern.MatchString(tag) {
		return corev1.PullIfNotPresent
	}
	return corev1.PullAlways
}

func imageTag(image string) string {
	lastSlash := strings.LastIndex(image, "/")
	lastColon := strings.LastIndex(image, ":")
	if lastColon <= lastSlash {
		return ""
	}
	return strings.TrimSpace(image[lastColon+1:])
}

func ensureVolume(volumes []corev1.Volume, v corev1.Volume) []corev1.Volume {
	for _, item := range volumes {
		if item.Name == v.Name {
			return volumes
		}
	}
	return append(volumes, v)
}

func ensureVolumeMount(mounts []corev1.VolumeMount, vm corev1.VolumeMount) []corev1.VolumeMount {
	for i := range mounts {
		if mounts[i].Name == vm.Name {
			if mounts[i].MountPath == "" {
				mounts[i].MountPath = vm.MountPath
			}
			return mounts
		}
	}
	return append(mounts, vm)
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}
