package kube

import "testing"

func TestPreparePodSpecAddsWorkspaceAndSyncthing(t *testing.T) {
	spec, err := PreparePodSpec(map[string]any{}, "ws-pvc", "/workspace", true, "syncthing:latest")
	if err != nil {
		t.Fatal(err)
	}
	containers, _ := spec["containers"].([]any)
	if len(containers) < 2 {
		t.Fatalf("expected at least 2 containers, got %d", len(containers))
	}
	if !hasContainer(containers, "syncthing") {
		t.Fatal("expected syncthing container")
	}
}

func TestPreparePodSpecWithoutSyncthing(t *testing.T) {
	spec, err := PreparePodSpec(map[string]any{}, "ws-pvc", "/workspace", false, "")
	if err != nil {
		t.Fatal(err)
	}
	containers, _ := spec["containers"].([]any)
	if hasContainer(containers, "syncthing") {
		t.Fatal("did not expect syncthing container")
	}
}
