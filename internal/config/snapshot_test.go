package config

import (
	"os"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestBuildWorkloadSnapshotPod(t *testing.T) {
	cfg := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Volumes:  []corev1.Volume{{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}}},
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
	if snap.Manifest != "" {
		t.Fatal("a pod workload with no manifest records none")
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
			Sync:     SyncSpec{Paths: []SyncPathSpec{{Local: ".", Remote: "/workspace"}}},
		},
	}
	cfg2 := &DevEnvironment{
		Spec: DevEnvSpec{
			Workload: WorkloadSpec{Type: "pod"},
			Sidecar:  SidecarSpec{Image: "img:1"},
			Ports:    []PortMapping{{Name: "grpc", Local: 9090, Remote: 90}},
			SSH:      SSHSpec{User: "bob"},
			Sync:     SyncSpec{Paths: []SyncPathSpec{{Local: "src/", Remote: "/workspace"}}},
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

func TestSnapshotRecordsTheManifestContent(t *testing.T) {
	f := t.TempDir() + "/job.yaml"
	os.WriteFile(f, []byte("apiVersion: batch/v1\nkind: Job\n"), 0o644)
	cfg := &DevEnvironment{Spec: DevEnvSpec{
		Workload: WorkloadSpec{Type: "job", ManifestPath: "job.yaml"},
		Sidecar:  SidecarSpec{Image: "img:1"},
	}}
	snap1 := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "job.yaml", f)
	if !strings.Contains(snap1.Manifest, "kind: Job") {
		t.Fatalf("the manifest itself must be recorded, got %q", snap1.Manifest)
	}

	// Editing it must still register as drift — the content replaces the digest
	// that used to serve that purpose, and also makes the change showable.
	os.WriteFile(f, []byte("apiVersion: batch/v1\nkind: Job\nmetadata:\n  name: changed\n"), 0o644)
	snap2 := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "job.yaml", f)
	h1, _ := snap1.SHA256()
	h2, _ := snap2.SHA256()
	if h1 == h2 {
		t.Fatal("a changed manifest must change the snapshot hash")
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
	if snap.Manifest == "" {
		t.Fatal("expected the manifest content for job workload")
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
	snap1.Manifest = "same"
	snap2.Manifest = "same"

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

// The pod root inject path is synthesized at apply time and does not change the
// resulting object. It must stay out of the snapshot: pods created before it
// existed recorded `Inject: null`, and stamping it would make every existing
// pod session report drift and demand a recreate on its next `okdev up`.
func TestSnapshotOmitsTheSynthesizedPodInject(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.SetDefaults()

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	if snap.Workload.Inject != nil {
		t.Fatalf("Workload.Inject = %+v, want nil for a default pod config", snap.Workload.Inject)
	}
	raw, err := snap.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, `"Inject":null`) {
		t.Fatalf("snapshot must record a null pod inject:\n%s", raw)
	}
}

// A user-configured inject is real intent and must still be captured.
func TestSnapshotKeepsAConfiguredInject(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workload.Type = "job"
	cfg.Spec.Workload.ManifestPath = "job.yaml"
	cfg.SetDefaults()

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "job.yaml", "")
	if len(snap.Workload.Inject) != 1 || snap.Workload.Inject[0].Path != "spec.template" {
		t.Fatalf("Workload.Inject = %+v, want the job's spec.template entry", snap.Workload.Inject)
	}
}

func TestSnapshotHashUnchangedForLegacySingularWorkload(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workload.Type = "pod"
	cfg.SetDefaults()

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	if snap.WorkloadProfile != "" {
		t.Fatalf("a desugared config must not stamp a profile name, got %q", snap.WorkloadProfile)
	}
	raw, err := snap.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "workloadProfile") {
		t.Fatalf("legacy snapshot JSON must not carry workloadProfile:\n%s", raw)
	}
}

func TestSnapshotCarriesProfileWhenDeclared(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workloads = []WorkloadProfile{{Name: "dev", Type: "pod"}, {Name: "big", Type: "pod", ManifestPath: "big.yaml"}}
	cfg.SetDefaults()
	if err := cfg.SelectWorkload("big"); err != nil {
		t.Fatal(err)
	}

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	if snap.WorkloadProfile != "big" {
		t.Fatalf("WorkloadProfile = %q, want big", snap.WorkloadProfile)
	}
}

func TestSnapshotHashDistinguishesSameTypeProfiles(t *testing.T) {
	build := func(profile string) string {
		cfg := &DevEnvironment{}
		cfg.Spec.Workloads = []WorkloadProfile{{Name: "dev", Type: "pod"}, {Name: "big", Type: "pod", ManifestPath: "big.yaml"}}
		cfg.SetDefaults()
		if err := cfg.SelectWorkload(profile); err != nil {
			t.Fatal(err)
		}
		snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
		h, err := snap.SHA256()
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	if build("dev") == build("big") {
		t.Fatal("two same-type profiles must not share a snapshot hash")
	}
}
