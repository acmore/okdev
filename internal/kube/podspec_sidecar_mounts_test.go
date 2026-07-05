package kube

import (
	"strings"
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

func TestPreparePodSpecAutoProvisionsSyncRemoteRoots(t *testing.T) {
	// Users configure extra sync mappings with paths only; okdev provisions
	// an emptyDir at any remote root not already covered by a volume, and
	// the sidecar mirror makes it servable by the remote syncthing.
	spec := corev1.PodSpec{
		Containers: []corev1.Container{{
			Name:  "dev",
			Image: "ubuntu:22.04",
			VolumeMounts: []corev1.VolumeMount{
				{Name: "shared-pvc", MountPath: "/shared"},
			},
		}},
	}
	volumes := []corev1.Volume{
		{Name: "shared-pvc", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "shared"}}},
	}
	prepared, err := PreparePodSpecForTargetWithShellAndSyncRoots(spec, volumes, "/workspace", "sidecar:test", corev1.ResourceRequirements{}, false, "", "dev", "", []string{
		"/data/results",     // uncovered -> emptyDir auto-provisioned
		"/shared/results",   // under the user PVC -> untouched
		"/workspace/nested", // under workspace -> untouched (also overlap-rejected upstream)
	})
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	var dev, sidecar *corev1.Container
	for i := range prepared.Containers {
		switch prepared.Containers[i].Name {
		case "dev":
			dev = &prepared.Containers[i]
		case "okdev-sidecar":
			sidecar = &prepared.Containers[i]
		}
	}
	devPaths := map[string]string{}
	for _, m := range dev.VolumeMounts {
		devPaths[m.MountPath] = m.Name
	}
	if name := devPaths["/data/results"]; name == "" || !strings.HasPrefix(name, "okdev-sync-") {
		t.Fatalf("expected auto-provisioned mount at /data/results, got %+v", dev.VolumeMounts)
	}
	if _, exists := devPaths["/shared/results"]; exists {
		t.Fatalf("covered root must not get its own mount, got %+v", dev.VolumeMounts)
	}
	if _, exists := devPaths["/workspace/nested"]; exists {
		t.Fatalf("workspace-nested root must not get its own mount, got %+v", dev.VolumeMounts)
	}
	volNames := map[string]bool{}
	for _, v := range prepared.Volumes {
		volNames[v.Name] = true
	}
	if !volNames[devPaths["/data/results"]] {
		t.Fatalf("expected emptyDir volume %q in pod spec", devPaths["/data/results"])
	}
	sidecarPaths := map[string]bool{}
	for _, m := range sidecar.VolumeMounts {
		sidecarPaths[m.MountPath] = true
	}
	if !sidecarPaths["/data/results"] {
		t.Fatalf("sidecar must mirror the auto-provisioned mount, got %+v", sidecar.VolumeMounts)
	}
}
