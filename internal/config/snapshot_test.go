package config

import (
	"os"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildWorkloadSnapshotPod(t *testing.T) {
	cfg := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			PodTemplate: PodTemplateRef{
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{{Name: "dev", Image: "ubuntu:22.04"}},
				},
			},
			Volumes: []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
			Sidecar: SidecarSpec{
				Image: "ghcr.io/acmore/okdev-sidecar:edge",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{
						corev1.ResourceCPU: resource.MustParse("250m"),
					},
				},
			},
		},
	}
	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", true, "", "echo bye", "", "")
	if snap.Version != "v1" {
		t.Fatalf("expected version v1, got %s", snap.Version)
	}
	if snap.WorkloadKind != "pod" {
		t.Fatalf("expected workloadKind pod, got %s", snap.WorkloadKind)
	}
	if snap.SidecarImage != "ghcr.io/acmore/okdev-sidecar:edge" {
		t.Fatalf("unexpected sidecarImage: %s", snap.SidecarImage)
	}
	if got := snap.SidecarResources.Requests.Cpu().String(); got != "250m" {
		t.Fatalf("unexpected sidecar cpu request: %s", got)
	}
	if snap.Tmux != true {
		t.Fatal("expected tmux true")
	}
	if snap.PreStop != "echo bye" {
		t.Fatalf("unexpected preStop: %s", snap.PreStop)
	}
	if snap.ManifestSHA256 != "" {
		t.Fatal("pod workload should have empty manifest hash")
	}
}

func TestWorkloadSnapshotHashIncludesSidecarResources(t *testing.T) {
	cfg1 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar: SidecarSpec{
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("250m")},
				},
			},
		},
	}
	cfg2 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar: SidecarSpec{
				Image: "img:1",
				Resources: corev1.ResourceRequirements{
					Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("500m")},
				},
			},
		},
	}
	snap1 := BuildWorkloadSnapshot(cfg1, "/workspace", "dev", false, "", "", "", "")
	snap2 := BuildWorkloadSnapshot(cfg2, "/workspace", "dev", false, "", "", "", "")
	h1, _ := snap1.SHA256()
	h2, _ := snap2.SHA256()
	if h1 == h2 {
		t.Fatal("expected sidecar resource changes to affect workload hash")
	}
}

func TestBuildWorkloadSnapshotExcludesNonWorkloadFields(t *testing.T) {
	cfg1 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar:  SidecarSpec{Image: "img:1"},
			Ports:    []PortMapping{{Name: "http", Local: 8080, Remote: 80}},
			SSH:      SSHSpec{User: "alice"},
			Sync:     SyncSpec{Paths: []string{"."}},
		},
	}
	cfg2 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar:  SidecarSpec{Image: "img:1"},
			Ports:    []PortMapping{{Name: "grpc", Local: 9090, Remote: 90}},
			SSH:      SSHSpec{User: "bob"},
			Sync:     SyncSpec{Paths: []string{"src/"}},
		},
	}
	snap1 := BuildWorkloadSnapshot(cfg1, "/workspace", "dev", false, "", "", "", "")
	snap2 := BuildWorkloadSnapshot(cfg2, "/workspace", "dev", false, "", "", "", "")
	h1, _ := snap1.SHA256()
	h2, _ := snap2.SHA256()
	if h1 != h2 {
		t.Fatal("snapshots should be equal when only non-workload fields differ")
	}
}

func TestBuildWorkloadSnapshotShellChangeAffectsHash(t *testing.T) {
	cfg1 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar:  SidecarSpec{Image: "img:1"},
			SSH:      SSHSpec{Shell: ""},
		},
	}
	cfg2 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar:  SidecarSpec{Image: "img:1"},
			SSH:      SSHSpec{Shell: "/bin/zsh"},
		},
	}
	snap1 := BuildWorkloadSnapshot(cfg1, "/workspace", "dev", false, "", "", "", "")
	snap2 := BuildWorkloadSnapshot(cfg2, "/workspace", "dev", false, "/bin/zsh", "", "", "")
	h1, _ := snap1.SHA256()
	h2, _ := snap2.SHA256()
	if h1 == h2 {
		t.Fatal("expected shell changes to affect workload hash")
	}
}

func TestComputeManifestSHA256(t *testing.T) {
	f := t.TempDir() + "/job.yaml"
	os.WriteFile(f, []byte("apiVersion: batch/v1\nkind: Job\n"), 0o644)
	h1, err := ComputeManifestSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == "" {
		t.Fatal("expected non-empty hash")
	}

	os.WriteFile(f, []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: changed\n"), 0o644)
	h2, err := ComputeManifestSHA256(f)
	if err != nil {
		t.Fatal(err)
	}
	if h1 == h2 {
		t.Fatal("expected different hash after content change")
	}
}

func TestBuildWorkloadSnapshotGenericIncludesManifestHash(t *testing.T) {
	f := t.TempDir() + "/job.yaml"
	os.WriteFile(f, []byte("apiVersion: batch/v1\nkind: Job\n"), 0o644)
	cfg := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "job", ManifestPath: f},
			Sidecar:  SidecarSpec{Image: "img:1"},
		},
	}
	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "job.yaml", f)
	if snap.ManifestSHA256 == "" {
		t.Fatal("expected manifest hash for job workload")
	}
	if snap.ManifestPath != "job.yaml" {
		t.Fatalf("unexpected manifest path: %s", snap.ManifestPath)
	}
}

func TestBuildWorkloadSnapshotUsesEffectiveInjectForInterPodSSH(t *testing.T) {
	enabled := true
	disabled := false
	cfg := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{
				Type:         "pytorchjob",
				ManifestPath: "pytorchjob.yaml",
				Inject: []WorkloadInjectSpec{
					{Path: "spec.pytorchReplicaSpecs.Master.template"},
					{Path: "spec.pytorchReplicaSpecs.Worker.template", Sidecar: &disabled},
				},
			},
			SSH: SSHSpec{InterPod: &enabled},
			Sidecar: SidecarSpec{
				Image: "img:1",
			},
		},
	}

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "pytorchjob.yaml", "")
	if len(snap.Workload.Inject) != 2 {
		t.Fatalf("expected 2 inject specs, got %d", len(snap.Workload.Inject))
	}
	if snap.Workload.Inject[1].Sidecar == nil || !*snap.Workload.Inject[1].Sidecar {
		t.Fatalf("expected effective snapshot inject sidecar to be enabled, got %+v", snap.Workload.Inject[1])
	}
}

func TestWorkloadSnapshotHashIgnoresManifestPath(t *testing.T) {
	cfg := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "job", ManifestPath: "job.yaml"},
			Sidecar:  SidecarSpec{Image: "img:1"},
		},
	}
	snap1 := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "job.yaml", "/tmp/a/job.yaml")
	snap2 := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "/Users/me/src/job.yaml", "/tmp/b/job.yaml")
	snap1.ManifestSHA256 = "same"
	snap2.ManifestSHA256 = "same"

	h1, err := snap1.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	h2, err := snap2.SHA256()
	if err != nil {
		t.Fatal(err)
	}
	if h1 != h2 {
		t.Fatalf("expected manifest path to be excluded from workload hash: %s != %s", h1, h2)
	}
}
