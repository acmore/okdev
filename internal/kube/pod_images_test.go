package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestContainerImagesFromPodReportsDeclaredNameAndResolvedDigest(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "dev", Image: "ubuntu:22.04"},
				{Name: "okdev-sidecar", Image: "okdev-sidecar:v0.8.0"},
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				// The kubelet fully-qualifies the reference in the status. That
				// form no longer matches what the user wrote, so the name comes
				// from the spec and only the digest — the part that actually
				// moves when a mutable tag moves — comes from here.
				{Name: "dev", Image: "docker.io/library/ubuntu:22.04", ImageID: "docker.io/library/ubuntu@sha256:1111111111112222222222223333333333334444444444445555555555556666"},
				{Name: "okdev-sidecar", Image: "docker.io/library/okdev-sidecar:v0.8.0", ImageID: "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd"},
			},
		},
	}

	images := containerImagesFromPod(pod)
	if len(images) != 2 {
		t.Fatalf("expected one entry per spec container, got %+v", images)
	}
	if images[0].Name != "dev" || images[1].Name != "okdev-sidecar" {
		t.Fatalf("spec order must be preserved, got %+v", images)
	}
	if images[0].Image != "ubuntu:22.04" {
		t.Fatalf("expected the declared reference, got %q", images[0].Image)
	}
	if images[0].Digest != "sha256:1111111111112222222222223333333333334444444444445555555555556666" {
		t.Fatalf("expected the digest from the repo@sha form, got %q", images[0].Digest)
	}
	if images[1].Digest != "sha256:abcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcdefabcd" {
		t.Fatalf("expected the bare digest form to pass through, got %q", images[1].Digest)
	}
}

func TestContainerImagesFromPodWithoutStatus(t *testing.T) {
	// A pending pod has no container statuses yet; the spec image is still the
	// answer to "what will this run", just without an immutable id.
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}}},
	}
	images := containerImagesFromPod(pod)
	if len(images) != 1 || images[0].Image != "ubuntu:22.04" || images[0].Digest != "" {
		t.Fatalf("unexpected images %+v", images)
	}
}

func TestImageDigestFromID(t *testing.T) {
	tests := map[string]string{
		"ghcr.io/org/dev@sha256:abc": "sha256:abc",
		"sha256:abc":                 "sha256:abc",
		// Some runtimes report a plain tag as the ImageID. That carries no
		// immutable identity, so it must not be presented as a digest.
		"docker.io/library/ubuntu:22.04": "",
		"":                               "",
	}
	for in, want := range tests {
		if got := imageDigestFromID(in); got != want {
			t.Fatalf("imageDigestFromID(%q) = %q, want %q", in, got, want)
		}
	}
}
