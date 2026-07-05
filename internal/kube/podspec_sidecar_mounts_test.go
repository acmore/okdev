package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func TestPreparePodSpecMirrorsTargetMountsIntoSidecar(t *testing.T) {
	// Sync mappings can target volume-backed remote roots outside the
	// workspace only if the sidecar mounts the same volumes: its syncthing
	// cannot serve paths on the target container's overlay filesystem.
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "dev",
			Image: "ubuntu:22.04",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "results-vol", MountPath: "/data"},
				{Name: "datasets", MountPath: "/data2", SubPath: "sets"},
			},
		}},
	}
	volumes := []corev1.Volume{
		{Name: "results-vol", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
		{Name: "datasets", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	prepared, err := PreparePodSpec(spec, volumes, "/workspace", "sidecar:test", corev1.ResourceRequirements{}, false, "")
	if err != nil {
		t.Fatalf("PreparePodSpec: %v", err)
	}
	var sidecar *corev1.Container
	for i := range prepared.Containers {
		if prepared.Containers[i].Name == "okdev-sidecar" {
			sidecar = &prepared.Containers[i]
		}
	}
	if sidecar == nil {
		t.Fatal("sidecar container missing")
	}
	byPath := map[string]corev1.VolumeMount{}
	for _, m := range sidecar.VolumeMounts {
		if _, dup := byPath[m.MountPath]; dup {
			t.Fatalf("duplicate sidecar mount path %q", m.MountPath)
		}
		byPath[m.MountPath] = m
	}
	for _, want := range []string{"/workspace", "/var/syncthing", "/var/okdev", "/data", "/data2"} {
		if _, ok := byPath[want]; !ok {
			t.Fatalf("expected sidecar mount at %q, got %+v", want, sidecar.VolumeMounts)
		}
	}
	if byPath["/data2"].SubPath != "sets" {
		t.Fatalf("expected subPath preserved on mirrored mount, got %+v", byPath["/data2"])
	}
}
