package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPreparePodSpecShareProcessNamespace(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	if spec.ShareProcessNamespace == nil || !*spec.ShareProcessNamespace {
		t.Fatal("expected ShareProcessNamespace to be true")
	}
}

func TestPreparePodSpecSidecarName(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	if !hasContainer(spec.Containers, "okdev-sidecar") {
		t.Fatal("expected okdev-sidecar container")
	}
}

func TestPreparePodSpecSidecarPorts(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			sidecar = c
			break
		}
	}
	wantPorts := map[int32]bool{22: false, 8384: false, 22000: false}
	for _, p := range sidecar.Ports {
		if _, ok := wantPorts[p.ContainerPort]; ok {
			wantPorts[p.ContainerPort] = true
		}
	}
	for port, found := range wantPorts {
		if !found {
			t.Fatalf("expected port %d on okdev-sidecar", port)
		}
	}
}

func TestPreparePodSpecSidecarVolumeMounts(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			sidecar = c
			break
		}
	}
	hasMount := func(name string) bool {
		for _, m := range sidecar.VolumeMounts {
			if m.Name == name {
				return true
			}
		}
		return false
	}
	if !hasMount("workspace") {
		t.Fatal("expected workspace volume mount on okdev-sidecar")
	}
	if !hasMount("syncthing-home") {
		t.Fatal("expected syncthing-home volume mount on okdev-sidecar")
	}
}

func TestPreparePodSpecSidecarPrivileged(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	var sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			sidecar = c
			break
		}
	}
	if sidecar.SecurityContext == nil || sidecar.SecurityContext.Privileged == nil || !*sidecar.SecurityContext.Privileged {
		t.Fatal("expected okdev-sidecar to run privileged for nsenter")
	}
}

func TestPreparePodSpecContainerCount(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Containers) != 2 {
		t.Fatalf("expected 2 containers (dev + okdev-sidecar), got %d", len(spec.Containers))
	}
}

func TestPreparePodSpecSidecarAlwaysAdded(t *testing.T) {
	// Even with syncthingEnabled=false, sidecar is still added.
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge")
	if err != nil {
		t.Fatal(err)
	}
	if !hasContainer(spec.Containers, "okdev-sidecar") {
		t.Fatal("expected okdev-sidecar container even when syncthingEnabled is false")
	}
}

func TestPreparePodSpecErrorsOnEmptyImage(t *testing.T) {
	_, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "")
	if err == nil {
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
