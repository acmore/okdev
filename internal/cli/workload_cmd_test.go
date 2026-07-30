package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
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

func TestAppendWorkloadProfilePreservesCommentsAndOrder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.yaml")
	original := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  # keep me: this comment must survive the edit
  namespace: default
  workloads:
    - name: dev
      type: pod
`
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}

	err := appendWorkloadProfileToConfig(path, config.WorkloadProfile{
		Name: "train", Type: "job", ManifestPath: "job.yaml",
	})
	if err != nil {
		t.Fatalf("appendWorkloadProfileToConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)
	if !strings.Contains(got, "# keep me") {
		t.Fatalf("the edit stripped comments:\n%s", got)
	}
	if !strings.Contains(got, "name: train") || !strings.Contains(got, "manifestPath: job.yaml") {
		t.Fatalf("the new profile was not appended:\n%s", got)
	}
	if !strings.Contains(got, "name: dev") {
		t.Fatalf("the edit dropped the existing profile:\n%s", got)
	}
}

func TestAppendWorkloadProfileRejectsDuplicateName(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  workloads:
    - name: dev
      type: pod
`), 0o644); err != nil {
		t.Fatal(err)
	}
	err := appendWorkloadProfileToConfig(path, config.WorkloadProfile{Name: "dev", Type: "job", ManifestPath: "j.yaml"})
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("expected a duplicate-name error, got %v", err)
	}
}

func TestAppendWorkloadProfileMaterializesLegacySingularWorkload(t *testing.T) {
	// A config that only has spec.workload must not lose the workload it is
	// already running when a second one is declared.
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  workload:
    type: job
    manifestPath: job.yaml
    inject:
      - path: spec.template
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkloadProfileToConfig(path, config.WorkloadProfile{
		Name: "train", Type: "pytorchjob", ManifestPath: "pt.yaml",
		Inject: []config.WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Worker.template"}},
	}); err != nil {
		t.Fatalf("appendWorkloadProfileToConfig: %v", err)
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("the rewritten config must still load: %v\n%s", err, raw)
	}
	names := cfg.WorkloadProfileNames()
	if len(names) != 2 || names[0] != config.DefaultWorkloadProfileName || names[1] != "train" {
		t.Fatalf("profiles = %v, want [default train]\n%s", names, raw)
	}
	if err := cfg.SelectWorkload(config.DefaultWorkloadProfileName); err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.Workload.Type != "job" || cfg.Spec.Workload.ManifestPath != "job.yaml" {
		t.Fatalf("the legacy workload was not preserved: %+v\n%s", cfg.Spec.Workload, raw)
	}
}

func TestAppendWorkloadProfileMaterializesImplicitPodWorkload(t *testing.T) {
	// `okdev init` omits spec.workload entirely for pod configs — pod is the
	// default. Declaring a second workload must still leave the pod one
	// declared, or the workload the session is running silently disappears.
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  podTemplate:
    spec:
      containers:
        - name: dev
          image: alpine
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := appendWorkloadProfileToConfig(path, config.WorkloadProfile{
		Name: "train", Type: "job", ManifestPath: "job.yaml",
	}); err != nil {
		t.Fatalf("appendWorkloadProfileToConfig: %v", err)
	}

	raw, _ := os.ReadFile(path)
	cfg, _, err := config.Load(path)
	if err != nil {
		t.Fatalf("the rewritten config must still load: %v\n%s", err, raw)
	}
	names := cfg.WorkloadProfileNames()
	if len(names) != 2 || names[0] != config.DefaultWorkloadProfileName || names[1] != "train" {
		t.Fatalf("profiles = %v, want [default train]\n%s", names, raw)
	}
	if err := cfg.SelectWorkload(config.DefaultWorkloadProfileName); err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.Workload.Type != "pod" || cfg.Spec.Workload.ManifestPath != "" {
		t.Fatalf("the implicit pod workload was not preserved: %+v\n%s", cfg.Spec.Workload, raw)
	}
	// It must still be a valid config: the pod profile keeps spec.podTemplate.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rewritten config must validate: %v\n%s", err, raw)
	}
}
