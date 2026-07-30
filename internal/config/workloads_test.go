package config

import (
	"strings"
	"testing"
)

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

func TestSelectWorkloadCollapsesIntoEffectiveWorkload(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workloads = []WorkloadProfile{
		{Name: "dev", Type: "pod"},
		{Name: "train", Type: "pytorchjob", ManifestPath: "pt.yaml",
			Inject: []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Worker.template"}},
			Attach: WorkloadAttachSpec{Container: "trainer"}},
	}
	cfg.SetDefaults()

	if err := cfg.SelectWorkload("train"); err != nil {
		t.Fatalf("SelectWorkload: %v", err)
	}
	if cfg.Spec.Workload.Type != "pytorchjob" {
		t.Fatalf("effective type = %q, want pytorchjob", cfg.Spec.Workload.Type)
	}
	if cfg.Spec.Workload.ManifestPath != "pt.yaml" {
		t.Fatalf("effective manifestPath = %q", cfg.Spec.Workload.ManifestPath)
	}
	if cfg.Spec.Workload.Attach.Container != "trainer" {
		t.Fatalf("effective attach = %+v", cfg.Spec.Workload.Attach)
	}
	if cfg.SelectedWorkload() != "train" {
		t.Fatalf("SelectedWorkload = %q, want train", cfg.SelectedWorkload())
	}
}

func TestSelectWorkloadFallsBackToDefaultThenFirst(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workloads = []WorkloadProfile{{Name: "dev", Type: "pod"}, {Name: "train", Type: "job", ManifestPath: "j.yaml"}}
	cfg.Spec.DefaultWorkload = "train"
	cfg.SetDefaults()
	if err := cfg.SelectWorkload(""); err != nil {
		t.Fatalf("SelectWorkload: %v", err)
	}
	if cfg.SelectedWorkload() != "train" {
		t.Fatalf("SelectedWorkload = %q, want train (defaultWorkload)", cfg.SelectedWorkload())
	}

	cfg2 := &DevEnvironment{}
	cfg2.Spec.Workloads = []WorkloadProfile{{Name: "dev", Type: "pod"}, {Name: "train", Type: "job", ManifestPath: "j.yaml"}}
	cfg2.SetDefaults()
	if err := cfg2.SelectWorkload(""); err != nil {
		t.Fatalf("SelectWorkload: %v", err)
	}
	if cfg2.SelectedWorkload() != "dev" {
		t.Fatalf("SelectedWorkload = %q, want dev (first entry)", cfg2.SelectedWorkload())
	}
}

func TestSelectWorkloadRejectsUnknownNameAndListsOptions(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workloads = []WorkloadProfile{{Name: "dev", Type: "pod"}, {Name: "train", Type: "job", ManifestPath: "j.yaml"}}
	cfg.SetDefaults()
	err := cfg.SelectWorkload("nope")
	if err == nil {
		t.Fatal("expected an error for an unknown profile")
	}
	for _, want := range []string{"nope", "dev", "train"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}
