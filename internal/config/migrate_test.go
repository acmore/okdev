package config

import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	sigyaml "sigs.k8s.io/yaml"
)

func loadFromBytes(data []byte) (*DevEnvironment, error) {
	var cfg DevEnvironment
	if err := sigyaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func TestRunMigrationsNoOp(t *testing.T) {
	input := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: test\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected 0 applied migrations, got %d", len(result.Applied))
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d", len(result.Warnings))
	}
}

func TestWorkspaceToVolumesMigration(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /code
    pvc:
      claimName: my-pvc
      size: 100Gi
      storageClassName: fast-ssd
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "workspace-to-volumes" {
		t.Fatalf("expected workspace-to-volumes applied, got %v", result.Applied)
	}

	// Verify the migrated YAML round-trips through config.Load
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}

	// Verify workspace key is removed
	if strings.Contains(string(out), "workspace:") {
		t.Fatal("expected workspace key to be removed from output")
	}

	// Verify volumes and podTemplate are present
	if !strings.Contains(string(out), "volumes:") {
		t.Fatal("expected volumes key in output")
	}
	if !strings.Contains(string(out), "podTemplate:") {
		t.Fatal("expected podTemplate key in output")
	}

	// Round-trip: load through sigs.k8s.io/yaml to verify compatibility
	cfg, err := loadFromBytes(out)
	if err != nil {
		t.Fatalf("migrated config failed to load: %v", err)
	}
	if len(cfg.Spec.Volumes) == 0 {
		t.Fatal("expected volumes after migration")
	}
	if cfg.WorkspaceMountPath() != "/code" {
		t.Fatalf("expected mount path /code, got %q", cfg.WorkspaceMountPath())
	}
}

func TestWorkspaceToVolumesIdempotent(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /code
    pvc:
      claimName: my-pvc
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	// Run twice
	if _, err := RunMigrations(&doc, DefaultMigrations); err != nil {
		t.Fatal(err)
	}
	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected 0 applied on second run, got %v", result.Applied)
	}
}

func TestWorkspaceToVolumesUnknownPVCKeys(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /workspace
    pvc:
      claimName: my-pvc
      unknownKey: some-value
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	// Should have a warning about unknown key
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "unknownKey") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning about unknownKey, got %v", result.Warnings)
	}
}
