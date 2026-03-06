package kube

import (
	"strings"
	"testing"
)

func TestBuildPVCManifest(t *testing.T) {
	m, err := BuildPVCManifest("dev", "pvc1", "10Gi", "", map[string]string{"okdev.io/managed": "true"}, map[string]string{"okdev.io/ttl-hours": "72"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "PersistentVolumeClaim") || !strings.Contains(s, "10Gi") || !strings.Contains(s, "okdev.io/ttl-hours") {
		t.Fatalf("unexpected manifest: %s", s)
	}
}

func TestBuildPodManifestDefault(t *testing.T) {
	m, err := BuildPodManifest("dev", "pod1", "pvc1", map[string]string{"okdev.io/managed": "true"}, map[string]string{"okdev.io/last-attach": "x"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "kind: Pod") || !strings.Contains(s, "claimName: pvc1") {
		t.Fatalf("unexpected pod manifest: %s", s)
	}
}
