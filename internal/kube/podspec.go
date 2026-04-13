package kube

import (
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var semverTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$`)

func PreparePodSpec(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string) (corev1.PodSpec, error) {
	return PreparePodSpecForTarget(podSpec, volumes, workspaceMountPath, sidecarImage, sidecarResources, tmux, preStop, "dev")
}

func PreparePodSpecForTarget(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string, targetContainer string) (corev1.PodSpec, error) {
	if strings.TrimSpace(sidecarImage) == "" {
		return corev1.PodSpec{}, fmt.Errorf("sidecar image cannot be empty")
	}
	if strings.TrimSpace(workspaceMountPath) == "" {
		workspaceMountPath = "/workspace"
	}
	if strings.TrimSpace(targetContainer) == "" {
		targetContainer = "dev"
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

	for _, v := range volumes {
		spec.Volumes = ensureVolume(spec.Volumes, v)
	}
	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name:         "syncthing-home",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})
	spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
		Name:         "okdev-runtime",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	})

	targetIndex := targetContainerIndex(spec.Containers, targetContainer)
	workspaceMount := corev1.VolumeMount{
		Name:      "workspace",
		MountPath: workspaceMountPath,
	}
	if targetIndex >= 0 {
		spec.Containers[targetIndex].VolumeMounts = ensureVolumeMount(spec.Containers[targetIndex].VolumeMounts, workspaceMount)
		workspaceMount = workspaceMountForSidecar(spec.Containers[targetIndex].VolumeMounts, workspaceMount)
		spec.Containers[targetIndex].VolumeMounts = ensureVolumeMount(spec.Containers[targetIndex].VolumeMounts, corev1.VolumeMount{
			Name:      "okdev-runtime",
			MountPath: "/var/okdev",
		})
		spec.Containers[targetIndex].Env = ensureEnvVar(spec.Containers[targetIndex].Env, corev1.EnvVar{
			Name:  "OKDEV_CONTAINER_ROLE",
			Value: "dev",
		})
		spec.Containers[targetIndex].Env = ensureEnvVar(spec.Containers[targetIndex].Env, corev1.EnvVar{
			Name:  "OKDEV_WORKSPACE",
			Value: workspaceMountPath,
		})
		if tmux {
			spec.Containers[targetIndex].Env = ensureEnvVar(spec.Containers[targetIndex].Env, corev1.EnvVar{
				Name:  "OKDEV_TMUX",
				Value: "1",
			})
		}
	}

	InjectPreStopForTarget(spec, preStop, targetContainer)

	if !hasContainer(spec.Containers, "okdev-sidecar") {
		privileged := true
		sidecarContainer := corev1.Container{
			Name:            "okdev-sidecar",
			Image:           sidecarImage,
			ImagePullPolicy: syncthingImagePullPolicy(sidecarImage),
			Resources:       sidecarResources,
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			Ports: []corev1.ContainerPort{
				{ContainerPort: 8384, Name: "st-gui"},
				{ContainerPort: 22000, Name: "st-sync"},
			},
			VolumeMounts: []corev1.VolumeMount{
				workspaceMount,
				{Name: "syncthing-home", MountPath: "/var/syncthing"},
				{Name: "okdev-runtime", MountPath: "/var/okdev"},
			},
			Env: []corev1.EnvVar{
				{
					Name:  "OKDEV_WORKSPACE",
					Value: workspaceMountPath,
				},
				{
					Name:  "OKDEV_CONTAINER_ROLE",
					Value: "sidecar",
				},
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

func InjectPreStopForTarget(spec *corev1.PodSpec, preStop string, targetContainer string) {
	if spec == nil || strings.TrimSpace(preStop) == "" {
		return
	}
	targetIndex := targetContainerIndex(spec.Containers, targetContainer)
	if targetIndex < 0 {
		return
	}
	if spec.Containers[targetIndex].Lifecycle == nil {
		spec.Containers[targetIndex].Lifecycle = &corev1.Lifecycle{}
	}
	spec.Containers[targetIndex].Lifecycle.PreStop = &corev1.LifecycleHandler{
		Exec: &corev1.ExecAction{
			Command: []string{"sh", "-c", preStop},
		},
	}
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

func workspaceMountForSidecar(mounts []corev1.VolumeMount, fallback corev1.VolumeMount) corev1.VolumeMount {
	for _, mount := range mounts {
		if mount.Name == fallback.Name && mount.MountPath == fallback.MountPath {
			return mount
		}
	}
	return fallback
}

func ensureEnvVar(envs []corev1.EnvVar, env corev1.EnvVar) []corev1.EnvVar {
	for i := range envs {
		if envs[i].Name == env.Name {
			if strings.TrimSpace(envs[i].Value) == "" {
				envs[i].Value = env.Value
			}
			return envs
		}
	}
	return append(envs, env)
}

func hasContainer(containers []corev1.Container, name string) bool {
	for _, c := range containers {
		if c.Name == name {
			return true
		}
	}
	return false
}

func targetContainerIndex(containers []corev1.Container, targetContainer string) int {
	for i := range containers {
		if containers[i].Name == targetContainer {
			return i
		}
	}
	if len(containers) > 0 {
		return 0
	}
	return -1
}
