package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func TestResolveWorkloadProfileNamePrefersFlagOverPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveWorkloadProfile("sess1", "train"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveWorkloadProfileName(&Options{Workload: "dev"}, "sess1")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "dev" {
		t.Fatalf("with --workload dev, got %q", got)
	}

	got, err = resolveWorkloadProfileName(&Options{}, "sess1")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "train" {
		t.Fatalf("without a flag the pin must win, got %q", got)
	}
}

func TestResolveWorkloadProfileNameEmptyWhenUnpinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := resolveWorkloadProfileName(&Options{}, "fresh")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "" {
		t.Fatalf("an unpinned session must resolve to \"\", got %q", got)
	}
}

func TestLabelsForSessionCarryTheWorkloadProfile(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "proj"
	cfg.Spec.Workloads = []config.WorkloadProfile{
		{Name: "dev", Type: "pod"},
		{Name: "train", Type: "job", ManifestPath: "j.yaml"},
	}
	cfg.SetDefaults()
	if err := cfg.SelectWorkload("train"); err != nil {
		t.Fatal(err)
	}

	labels := labelsForSession(&Options{}, cfg, "sess1")
	if labels["okdev.io/workload-profile"] != "train" {
		t.Fatalf("workload-profile = %q, want train", labels["okdev.io/workload-profile"])
	}
	if labels["okdev.io/workload-type"] != "job" {
		t.Fatalf("workload-type = %q, want job", labels["okdev.io/workload-type"])
	}
}

// A pod created before the profile label existed carries none. It is still the
// live workload, so LIVE must mark it — otherwise `workload list` shows PINNED
// without LIVE on a perfectly healthy session, which the docs define as "the
// switch has not been applied yet".
func TestLiveWorkloadProfileFallsBackToTypeForUnlabelledPods(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.SetDefaults()

	pods := []kube.PodSummary{{
		Name:   "okdev-legacy-ab9c784b",
		Labels: map[string]string{"okdev.io/workload-type": "pod"},
	}}
	if got := liveProfileFromPods(cfg, pods); got != config.DefaultWorkloadProfileName {
		t.Fatalf("live = %q, want %q for an unlabelled pod of the selected type", got, config.DefaultWorkloadProfileName)
	}

	// A label-less pod of a *different* type is not this profile — that is a
	// pending switch, and LIVE must stay blank.
	other := []kube.PodSummary{{
		Name:   "okdev-legacy-ab9c784b",
		Labels: map[string]string{"okdev.io/workload-type": "job"},
	}}
	if got := liveProfileFromPods(cfg, other); got != "" {
		t.Fatalf("live = %q, want empty for an unlabelled pod of another type", got)
	}
}

func TestLiveWorkloadProfilePrefersTheLabel(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Workloads = []config.WorkloadProfile{
		{Name: "dev", Type: "pod"},
		{Name: "train", Type: "job", ManifestPath: "j.yaml"},
	}
	cfg.SetDefaults()

	pods := []kube.PodSummary{{
		Name:   "okdev-x",
		Labels: map[string]string{"okdev.io/workload-profile": "train", "okdev.io/workload-type": "job"},
	}}
	if got := liveProfileFromPods(cfg, pods); got != "train" {
		t.Fatalf("live = %q, want train", got)
	}
}

func TestWorkloadGroupHasNoAddSubcommand(t *testing.T) {
	// Declaring a workload belongs to `okdev init`; this group only inspects
	// and switches. Two commands for one job is what shipped broken in v0.9.0.
	for _, sub := range newWorkloadCmd(&Options{}).Commands() {
		if sub.Name() == "add" {
			t.Fatal("okdev workload add must be gone; use okdev init --workload-name")
		}
	}
}
