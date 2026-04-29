package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type migrateTemplateFixture struct {
	configPath   string
	manifestPath string
}

// writeMigrateTemplateFixture writes a throw-away project under t.TempDir and
// os.Chdir's into it. Callers MUST NOT run in parallel (no t.Parallel()) since
// tests share the process-wide working directory.
func writeMigrateTemplateFixture(t *testing.T) migrateTemplateFixture {
	t.Helper()

	tmp := t.TempDir()
	tmplDir := filepath.Join(tmp, ".okdev", "templates")
	if err := os.MkdirAll(filepath.Join(tmplDir, "manifests"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "pytorch.yaml.tmpl"), []byte(`---
name: pytorch
variables:
  - name: image
    type: string
  - name: manifestPath
    type: string
    default: pytorchjob.yaml
files:
  - path: "{{ .Vars.manifestPath }}"
    template: manifests/pytorchjob.yaml.tmpl
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  sync:
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  template:
    name: pytorch
    vars:
      image: {{ .Vars.image }}
      manifestPath: {{ .Vars.manifestPath }}
  workload:
    type: pytorchjob
    manifestPath: {{ .Vars.manifestPath }}
    inject:
      - path: "spec.pytorchReplicaSpecs.Master.template"
      - path: "spec.pytorchReplicaSpecs.Worker.template"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "manifests", "pytorchjob.yaml.tmpl"), []byte(`apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: {{ .Name }}
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: dev
              image: {{ .Vars.image }}
              volumeMounts:
                - name: workspace
                  mountPath: {{ .SyncRemote }}
    Worker:
      template:
        spec:
          containers:
            - name: dev
              image: {{ .Vars.image }}
              volumeMounts:
                - name: workspace
                  mountPath: {{ .SyncRemote }}
`), 0o644); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(tmp, ".okdev", "okdev.yaml")
	manifestPath := filepath.Join(tmp, ".okdev", "pytorchjob.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    paths:
      - ".:/train"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  template:
    name: pytorch
    vars:
      image: stale:old
      manifestPath: pytorchjob.yaml
  workload:
    type: pytorchjob
    manifestPath: pytorchjob.yaml
    inject:
      - path: "spec.pytorchReplicaSpecs.Master.template"
      - path: "spec.pytorchReplicaSpecs.Worker.template"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, []byte("image: stale:old\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	oldwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	return migrateTemplateFixture{
		configPath:   configPath,
		manifestPath: manifestPath,
	}
}

func TestMigrateTemplateRewritesDeclaredFilesAndBacksThemUp(t *testing.T) {
	fixture := writeMigrateTemplateFixture(t)

	cmd := newMigrateCmd(&Options{ConfigPath: fixture.configPath})
	cmd.SetArgs([]string{"--template", "pytorch", "--yes", "--set", "image=fresh:new"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate execute: %v", err)
	}

	manifestRaw, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestRaw), "image: fresh:new") {
		t.Fatalf("expected rewritten manifest image, got:\n%s", string(manifestRaw))
	}
	if !strings.Contains(string(manifestRaw), "mountPath: /train") {
		t.Fatalf("expected rewritten manifest to preserve SyncRemote from config, got:\n%s", string(manifestRaw))
	}

	manifestBak, err := os.ReadFile(fixture.manifestPath + ".bak")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestBak), "image: stale:old") {
		t.Fatalf("expected manifest backup to keep stale content, got:\n%s", string(manifestBak))
	}

	if !strings.Contains(out.String(), "Rewrote companion file "+fixture.manifestPath) {
		t.Fatalf("expected success output to mention companion rewrite, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "backup: "+fixture.manifestPath+".bak") {
		t.Fatalf("expected success output to mention companion backup, got:\n%s", out.String())
	}
}

func TestMigrateTemplateReconstructsBuiltInTemplateVarsFromConfig(t *testing.T) {
	fixture := writeMigrateTemplateFixture(t)

	cmd := newMigrateCmd(&Options{ConfigPath: fixture.configPath})
	cmd.SetArgs([]string{"--template", "pytorch", "--yes", "--set", "image=fresh:new"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate execute: %v", err)
	}

	manifestRaw, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestRaw), "mountPath: /train") {
		t.Fatalf("expected migrate to reconstruct built-in template vars from config, got:\n%s", string(manifestRaw))
	}
	if strings.Contains(string(manifestRaw), "mountPath: /workspace") {
		t.Fatalf("expected migrate to avoid default SyncRemote, got:\n%s", string(manifestRaw))
	}
}

func TestMigrateTemplateDryRunReportsCompanionFilesWithoutWriting(t *testing.T) {
	fixture := writeMigrateTemplateFixture(t)

	cmd := newMigrateCmd(&Options{ConfigPath: fixture.configPath})
	cmd.SetArgs([]string{"--template", "pytorch", "--yes", "--dry-run", "--set", "image=fresh:new"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate execute: %v", err)
	}

	if !strings.Contains(out.String(), "Would rewrite companion files:") {
		t.Fatalf("expected dry-run summary to mention companion files, got:\n%s", out.String())
	}
	if !strings.Contains(out.String(), fixture.manifestPath) {
		t.Fatalf("expected dry-run summary to mention %q, got:\n%s", fixture.manifestPath, out.String())
	}

	manifestRaw, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestRaw), "image: stale:old") {
		t.Fatalf("expected dry-run to keep stale manifest content, got:\n%s", string(manifestRaw))
	}
	if _, err := os.Stat(fixture.manifestPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("expected no companion backup on dry-run, err=%v", err)
	}
}

func TestMigrateTemplateNoBackupSkipsCompanionBackups(t *testing.T) {
	fixture := writeMigrateTemplateFixture(t)

	cmd := newMigrateCmd(&Options{ConfigPath: fixture.configPath})
	cmd.SetArgs([]string{"--template", "pytorch", "--yes", "--no-backup", "--set", "image=fresh:new"})
	cmd.SetIn(strings.NewReader(""))
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("migrate execute: %v", err)
	}

	manifestRaw, err := os.ReadFile(fixture.manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(manifestRaw), "image: fresh:new") {
		t.Fatalf("expected rewritten manifest image, got:\n%s", string(manifestRaw))
	}
	if _, err := os.Stat(fixture.manifestPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("expected no companion backup when --no-backup is set, err=%v", err)
	}
	if _, err := os.Stat(fixture.configPath + ".bak"); !os.IsNotExist(err) {
		t.Fatalf("expected no config backup when --no-backup is set, err=%v", err)
	}
}
