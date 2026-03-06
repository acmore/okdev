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
