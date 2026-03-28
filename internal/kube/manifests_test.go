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

func TestBuildPVCManifestDefaultsSizeAndStorageClass(t *testing.T) {
	m, err := BuildPVCManifest("dev", "pvc1", "", "fast", nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "storage: 50Gi") {
		t.Fatalf("expected default pvc size in manifest: %s", s)
	}
	if !strings.Contains(s, "storageClassName: fast") {
		t.Fatalf("expected storage class in manifest: %s", s)
	}
}

func TestBuildPVCManifestRejectsInvalidSize(t *testing.T) {
	if _, err := BuildPVCManifest("dev", "pvc1", "not-a-size", "", nil, nil); err == nil {
		t.Fatal("expected invalid pvc size error")
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

func TestBuildPodManifestIncludesLabelsAndAnnotations(t *testing.T) {
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}},
	}
	m, err := BuildPodManifest("dev", "pod1", map[string]string{"app": "demo"}, map[string]string{"anno": "value"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	s := string(m)
	if !strings.Contains(s, "app: demo") || !strings.Contains(s, "anno: value") {
		t.Fatalf("expected labels and annotations in manifest: %s", s)
	}
}
