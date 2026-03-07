package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPreparePodSpecAddsWorkspaceAndSyncthing(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", true, "syncthing:latest")
	if err != nil {
		t.Fatal(err)
	}
	if len(spec.Containers) < 2 {
		t.Fatalf("expected at least 2 containers, got %d", len(spec.Containers))
	}
	if !hasContainer(spec.Containers, "syncthing") {
		t.Fatal("expected syncthing container")
	}
	var st corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "syncthing" {
			st = c
			break
		}
	}
	if st.ImagePullPolicy != corev1.PullAlways {
		t.Fatalf("expected PullAlways for mutable tag, got %s", st.ImagePullPolicy)
	}
}

func TestPreparePodSpecWithoutSyncthing(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", false, "")
	if err != nil {
		t.Fatal(err)
	}
	if hasContainer(spec.Containers, "syncthing") {
		t.Fatal("did not expect syncthing container")
	}
}

func TestPreparePodSpecWithSSHSidecar(t *testing.T) {
	spec, err := PreparePodSpecWithSSH(corev1.PodSpec{}, "ws-pvc", "/workspace", false, "", true, "ghcr.io/acmore/okdev-sshd:edge")
	if err != nil {
		t.Fatal(err)
	}
	if !hasContainer(spec.Containers, "okdev-ssh") {
		t.Fatal("expected okdev-ssh container")
	}
}

func TestPreparePodSpecWithSSHErrorsOnEmptyImage(t *testing.T) {
	_, err := PreparePodSpecWithSSH(corev1.PodSpec{}, "ws-pvc", "/workspace", false, "", true, "")
	if err == nil {
		t.Fatal("expected error for empty ssh sidecar image")
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
