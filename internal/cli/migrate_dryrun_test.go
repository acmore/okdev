package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --dry-run has to show the manifests, not just the config: for this migration
// the manifests are most of what changes, and the one a user needs to inspect
// is the hand-written one okdev is about to edit.
func TestMigrateDryRunShowsManifestChangesAndWritesNothing(t *testing.T) {
	dir := t.TempDir()
	okdevDir := filepath.Join(dir, ".okdev")
	if err := os.MkdirAll(okdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgPath := filepath.Join(okdevDir, "okdev.yaml")
	cfg := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  volumes:
    - name: datasets
      persistentVolumeClaim:
        claimName: ds
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  workload:
    type: pod
    manifestPath: pod.yaml
`
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: alpine
`
	manifestPath := filepath.Join(okdevDir, "pod.yaml")
	if err := os.WriteFile(cfgPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newMigrateCmd(&Options{ConfigPath: cfgPath})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetArgs([]string{"--dry-run"})
	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate --dry-run: %v", err)
	}

	got := out.String()
	if !strings.Contains(got, "pod.yaml (edited)") {
		t.Fatalf("dry-run must name the manifest it would edit:\n%s", got)
	}
	// Shown as a diff, so the change is readable without eyeballing a whole file.
	if !strings.Contains(got, "+     volumes:") && !strings.Contains(got, "+   volumes:") {
		t.Fatalf("dry-run must show the added volumes:\n%s", got)
	}

	// And nothing on disk moved.
	if after, _ := os.ReadFile(manifestPath); string(after) != manifest {
		t.Fatalf("dry-run rewrote the manifest:\n%s", after)
	}
	if after, _ := os.ReadFile(cfgPath); string(after) != cfg {
		t.Fatalf("dry-run rewrote the config:\n%s", after)
	}
	if _, err := os.Stat(cfgPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("dry-run wrote a backup, err=%v", err)
	}
}
