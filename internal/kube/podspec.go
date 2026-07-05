package kube

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	corev1 "k8s.io/api/core/v1"
)

var semverTagPattern = regexp.MustCompile(`^v?\d+\.\d+\.\d+([.-][0-9A-Za-z.-]+)?$`)

func PreparePodSpec(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string) (corev1.PodSpec, error) {
	return PreparePodSpecForTargetWithShell(podSpec, volumes, workspaceMountPath, sidecarImage, sidecarResources, tmux, preStop, "dev", "")
}

func PreparePodSpecForTarget(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string, targetContainer string) (corev1.PodSpec, error) {
	return PreparePodSpecForTargetWithShell(podSpec, volumes, workspaceMountPath, sidecarImage, sidecarResources, tmux, preStop, targetContainer, "")
}

func PreparePodSpecForTargetWithShell(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string, targetContainer string, shell string) (corev1.PodSpec, error) {
	return PreparePodSpecForTargetWithShellAndSyncRoots(podSpec, volumes, workspaceMountPath, sidecarImage, sidecarResources, tmux, preStop, targetContainer, shell, nil)
}

// PreparePodSpecForTargetWithShellAndSyncRoots additionally auto-provisions
// volumes for sync mapping remote roots: any root not already covered by a
// target-container volume mount gets an emptyDir mounted at that exact path
// (and mirrored into the sidecar), so users configure extra sync mappings
// without writing any volume YAML. Roots covered by an existing mount (for
// example a user PVC) are left alone.
func PreparePodSpecForTargetWithShellAndSyncRoots(podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, sidecarResources corev1.ResourceRequirements, tmux bool, preStop string, targetContainer string, shell string, syncRemoteRoots []string) (corev1.PodSpec, error) {
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
		if strings.TrimSpace(shell) != "" {
			spec.Containers[targetIndex].Env = ensureEnvVar(spec.Containers[targetIndex].Env, corev1.EnvVar{
				Name:  "OKDEV_SHELL",
				Value: shell,
			})
		}
	}

	if targetIndex >= 0 {
		for _, root := range syncRemoteRoots {
			root = strings.TrimSpace(root)
			if root == "" || syncRootCoveredByMounts(root, spec.Containers[targetIndex].VolumeMounts) {
				continue
			}
			name := syncAutoVolumeName(root)
			spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
				Name:         name,
				VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
			})
			spec.Containers[targetIndex].VolumeMounts = ensureVolumeMount(spec.Containers[targetIndex].VolumeMounts, corev1.VolumeMount{
				Name:      name,
				MountPath: root,
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
			VolumeMounts: sidecarVolumeMounts(workspaceMount, targetIndex, spec.Containers),
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

// syncRootCoveredByMounts reports whether root already lives under one of
// the container's volume mounts.
func syncRootCoveredByMounts(root string, mounts []corev1.VolumeMount) bool {
	for _, m := range mounts {
		if root == m.MountPath || strings.HasPrefix(root, m.MountPath+"/") {
			return true
		}
	}
	return false
}

// syncAutoVolumeName derives a stable volume name for an auto-provisioned
// sync remote root.
func syncAutoVolumeName(root string) string {
	sum := sha256.Sum256([]byte(root))
	return "okdev-sync-" + hex.EncodeToString(sum[:4])
}

func workspaceMountForSidecar(mounts []corev1.VolumeMount, fallback corev1.VolumeMount) corev1.VolumeMount {
	for _, mount := range mounts {
		if mount.Name == fallback.Name && mount.MountPath == fallback.MountPath {
			return mount
		}
	}
	return fallback
}

// sidecarVolumeMounts assembles the sidecar's mounts: its internal volumes,
// the workspace, plus every volume the target container mounts (at the same
// paths). Mirroring the target's mounts is what lets sync mappings target
// volume-backed remote roots outside the workspace — the sidecar's syncthing
// can only serve paths it can see; anything on the target container's
// overlay filesystem is invisible to it.
func sidecarVolumeMounts(workspaceMount corev1.VolumeMount, targetIndex int, containers []corev1.Container) []corev1.VolumeMount {
	mounts := []corev1.VolumeMount{
		workspaceMount,
		{Name: "syncthing-home", MountPath: "/var/syncthing"},
		{Name: "okdev-runtime", MountPath: "/var/okdev"},
	}
	if targetIndex < 0 || targetIndex >= len(containers) {
		return mounts
	}
	taken := make(map[string]bool, len(mounts))
	for _, m := range mounts {
		taken[m.MountPath] = true
	}
	for _, m := range containers[targetIndex].VolumeMounts {
		if taken[m.MountPath] {
			continue
		}
		taken[m.MountPath] = true
		mounts = append(mounts, m)
	}
	return mounts
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
