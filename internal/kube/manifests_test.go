package kube

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestBuildPVCManifest(t *testing.T) {
	m, err := BuildPVCManifest("dev", "pvc1", "10Gi", "", map[string]string{"okdev.io/managed": "true"}, map[string]string{"okdev.io/ttl-hours": "72"})
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "kind: PersistentVolumeClaim") || !strings.Contains(s, "apiVersion: v1") || !strings.Contains(s, "ReadWriteOnce") || !strings.Contains(s, "okdev.io/ttl-hours") {
		t.Fatalf("unexpected manifest: %s", s)
	}
}

func TestBuildPodManifestDefault(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}},
	}
	m, err := BuildPodManifest("dev", "pod1", map[string]string{"okdev.io/managed": "true"}, map[string]string{"okdev.io/last-attach": "x"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "kind: Pod") || !strings.Contains(s, "apiVersion: v1") || !strings.Contains(s, "name: pod1") {
		t.Fatalf("unexpected pod manifest: %s", s)
	}
}
