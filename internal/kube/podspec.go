package kube

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var semverTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$`)

func PreparePodSpec(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath, sidecarImage string, tmux bool) (corev1.PodSpec, error) {
	if strings.TrimSpace(sidecarImage) == "" {
		return corev1.PodSpec{}, fmt.Errorf("sidecar image cannot be empty")
	}

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

	shareProcessNamespace := true
	spec.ShareProcessNamespace = &shareProcessNamespace

	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name: "workspace",
		VolumeSource: corev1.VolumeSource{
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspaceClaim},
		},
	})
	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name:         "syncthing-home",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	for i := range spec.Containers {
		spec.Containers[i].VolumeMounts = ensureVolumeMount(spec.Containers[i].VolumeMounts, corev1.VolumeMount{
			Name:      "workspace",
			MountPath: workspaceMountPath,
		})
	}

	if !hasContainer(spec.Containers, "okdev-sidecar") {
		privileged := true
		sidecarContainer := corev1.Container{
			Name:            "okdev-sidecar",
			Image:           sidecarImage,
			ImagePullPolicy: syncthingImagePullPolicy(sidecarImage),
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			Ports: []corev1.ContainerPort{
				{ContainerPort: 22, Name: "ssh"},
				{ContainerPort: 8384, Name: "st-gui"},
				{ContainerPort: 22000, Name: "st-sync"},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: workspaceMountPath},
				{Name: "syncthing-home", MountPath: "/var/syncthing"},
			},
		}
		if tmux {
			sidecarContainer.Env = append(sidecarContainer.Env, corev1.EnvVar{
				Name:  "OKDEV_TMUX",
				Value: "1",
			})
		}
		spec.Containers = append(spec.Containers, sidecarContainer)
	}

	return *spec, nil
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
