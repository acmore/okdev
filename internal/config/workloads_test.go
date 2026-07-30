package config

import "testing"

func TestSingularWorkloadDesugarsIntoOneProfile(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workload.Type = "job"
	cfg.Spec.Workload.ManifestPath = "job.yaml"
	cfg.SetDefaults()

	if len(cfg.Spec.Workloads) != 1 {
		t.Fatalf("workloads = %d, want 1", len(cfg.Spec.Workloads))
	}
	got := cfg.Spec.Workloads[0]
	if got.Name != DefaultWorkloadProfileName {
		t.Fatalf("name = %q, want %q", got.Name, DefaultWorkloadProfileName)
	}
	if got.Type != "job" || got.ManifestPath != "job.yaml" {
		t.Fatalf("profile did not carry the singular workload: %+v", got)
	}
	if cfg.DeclaresWorkloadProfiles() {
		t.Fatal("a desugared config must not report declared profiles")
	}
}

func TestDeclaredWorkloadsAreKept(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workloads = []WorkloadProfile{
		{Name: "dev", Type: "pod"},
		{Name: "train", Type: "pytorchjob", ManifestPath: "pt.yaml",
			Inject: []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Worker.template"}}},
	}
	cfg.SetDefaults()

	if len(cfg.Spec.Workloads) != 2 {
		t.Fatalf("workloads = %d, want 2", len(cfg.Spec.Workloads))
	}
	if !cfg.DeclaresWorkloadProfiles() {
		t.Fatal("a config with spec.workloads must report declared profiles")
	}
}

func TestEmptyConfigDesugarsToADefaultPodProfile(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.SetDefaults()
	if len(cfg.Spec.Workloads) != 1 || cfg.Spec.Workloads[0].Type != "pod" {
		t.Fatalf("workloads = %+v, want one pod profile", cfg.Spec.Workloads)
	}
}
