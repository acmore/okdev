package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPreparePodSpecShareProcessNamespace(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Fatal("expected ShareProcessNamespace=true")
	}
}

func TestPreparePodSpecWorkspaceDefaultsToEmptyDir(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Volumes {
		if v.Name == "workspace" {
			if v.EmptyDir == nil {
				t.Fatal("expected workspace emptyDir when no workspace volume configured")
			}
			return
		}
	}
	t.Fatal("workspace volume not found")
}

func TestPreparePodSpecUsesConfiguredWorkspaceVolume(t *testing.T) {
	volumes := []corev1.Volume{
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
					ClaimName: "team-workspace",
				},
			},
		},
	}
	spec, err := PreparePodSpec(corev1.PodSpec{}, volumes, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "")
	if err != nil {
		t.Fatal(err)
	}
	for _, v := range spec.Volumes {
		if v.Name == "workspace" {
			if v.PersistentVolumeClaim == nil || v.PersistentVolumeClaim.ClaimName != "team-workspace" {
				t.Fatalf("expected workspace pvc claim team-workspace, got %#v", v.PersistentVolumeClaim)
			}
			return
		}
	}
	t.Fatal("workspace volume not found")
}

func TestPreparePodSpecAddsWorkspaceMountOnDevAndSidecar(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "")
	if err != nil {
		t.Fatal(err)
	}
	var dev, sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "dev" {
			dev = c
		}
		if c.Name == "okdev-sidecar" {
			sidecar = c
		}
	}
	hasMount := func(c corev1.Container, name, path string) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == name && m.MountPath == path {
				return true
			}
		}
		return false
	}
	if !hasMount(dev, "workspace", "/workspace") {
		t.Fatal("expected workspace mount on dev")
	}
	if !hasMount(sidecar, "workspace", "/workspace") {
		t.Fatal("expected workspace mount on sidecar")
	}
	if !hasMount(dev, "okdev-runtime", "/var/okdev") {
		t.Fatal("expected okdev runtime mount on dev")
	}
	if !hasMount(sidecar, "okdev-runtime", "/var/okdev") {
		t.Fatal("expected okdev runtime mount on sidecar")
	}
}

func TestPreparePodSpecErrorsOnEmptyImage(t *testing.T) {
	if _, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "", false, ""); err == nil {
		t.Fatal("expected error for empty sidecar image")
	}
}

func TestSyncthingImagePullPolicy(t *testing.T) {
	cases := []struct {
		image string
		want  corev1.PullPolicy
	}{
		{image: "ghcr.io/acmore/okdev:edge", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev:latest", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev", want: corev1.PullAlways},
		{image: "ghcr.io/acmore/okdev:v0.1.0", want: corev1.PullIfNotPresent},
		{image: "ghcr.io/acmore/okdev:1.2.3", want: corev1.PullIfNotPresent},
		{image: "ghcr.io/acmore/okdev@sha256:deadbeef", want: corev1.PullIfNotPresent},
	}
	for _, tc := range cases {
		if got := syncthingImagePullPolicy(tc.image); got != tc.want {
			t.Fatalf("image %q: want %s, got %s", tc.image, tc.want, got)
		}
	}
}

func findContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if containers[i].Name == name {
			return &containers[i]
		}
	}
	return nil
}

func TestPreparePodSpecSetsDevTmuxEnvWhenEnabled(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", true, "")
	if err != nil {
		t.Fatal(err)
	}
	dev := findContainer(spec.Containers, "dev")
	if dev == nil {
		t.Fatal("dev container not found")
	}
	for _, env := range dev.Env {
		if env.Name == "OKDEV_TMUX" && env.Value == "1" {
			return
		}
	}
	t.Fatal("expected OKDEV_TMUX=1 on dev container when tmux is enabled")
}

func TestPreparePodSpecForTargetUsesNamedContainer(t *testing.T) {
	spec, err := PreparePodSpecForTarget(corev1.PodSpec{
		Containers: []corev1.Container{
			{Name: "trainer", Image: "python:3.12"},
			{Name: "helper", Image: "busybox"},
		},
	}, nil, "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false, "echo bye", "trainer")
	if err != nil {
		t.Fatal(err)
	}
	trainer := findContainer(spec.Containers, "trainer")
	if trainer == nil {
		t.Fatal("trainer container not found")
	}
	helper := findContainer(spec.Containers, "helper")
	if helper == nil {
		t.Fatal("helper container not found")
	}
	hasMount := func(c *corev1.Container, name string) bool {
		for _, m := range c.VolumeMounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}
	if !hasMount(trainer, "workspace") || !hasMount(trainer, "okdev-runtime") {
		t.Fatalf("expected mounts on trainer, got %+v", trainer.VolumeMounts)
	}
	if hasMount(helper, "workspace") || hasMount(helper, "okdev-runtime") {
		t.Fatalf("did not expect runtime mounts on helper, got %+v", helper.VolumeMounts)
	}
	if trainer.Lifecycle == nil || trainer.Lifecycle.PreStop == nil {
		t.Fatal("expected prestop lifecycle on trainer container")
	}
}
