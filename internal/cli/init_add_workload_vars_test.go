package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

// A template that declares variables is the normal case for anything beyond a
// starter pod: the body branches on them and the companion manifest consumes
// them. Adding such a workload to an existing config has to resolve them the
// same way creating a fresh config does.
const varTemplateSource = `---
name: trainer
description: template with variables
variables:
  - name: manifestPath
    type: string
    default: trainer.yaml
  - name: workerReplicas
    type: int
    default: 1
files:
  - path: "{{ .Vars.manifestPath }}"
    template: manifests/trainer.yaml.tmpl
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  workload:
    type: pytorchjob
    manifestPath: {{ .Vars.manifestPath }}
    inject:
      - path: "spec.pytorchReplicaSpecs.Master.template"
{{- if gt .Vars.workerReplicas 0 }}
      - path: "spec.pytorchReplicaSpecs.Worker.template"
{{- end }}
    attach:
      container: dev
`

const varManifestSource = `apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: trainer
spec:
  pytorchReplicaSpecs:
    Master:
      replicas: 1
      template:
        spec:
          containers:
            - name: dev
              image: ubuntu:22.04
    Worker:
      replicas: {{ .Vars.workerReplicas }}
      template:
        spec:
          containers:
            - name: dev
              image: ubuntu:22.04
`

func writeVarTemplate(t *testing.T, dir string) {
	t.Helper()
	manifestDir := filepath.Join(dir, ".okdev", "templates", "manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmpl := filepath.Join(dir, ".okdev", "templates", "trainer.yaml.tmpl")
	if err := os.WriteFile(tmpl, []byte(varTemplateSource), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "trainer.yaml.tmpl"), []byte(varManifestSource), 0o644); err != nil {
		t.Fatal(err)
	}
}

// The declared defaults are the template's own answer for every variable it
// takes. Adding a workload without --set must use them rather than render
// against an empty .Vars, which failed with a raw Go template error.
func TestInitAddsAWorkloadFromATemplateWithVariableDefaults(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir, "--yes", "--name", "proj", "--namespace", "default"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	writeVarTemplate(t, dir)

	if _, err := runInit(t, dir, "--yes", "--template", "trainer", "--workload-name", "train"); err != nil {
		t.Fatalf("additive init with a variable template: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, ".okdev", "train.yaml"))
	if err != nil {
		t.Fatalf("the manifest must be scaffolded: %v", err)
	}
	if !strings.Contains(string(manifest), "replicas: 1") {
		t.Fatalf("the declared default must reach the manifest:\n%s", manifest)
	}
}

// --set is how a template variable is chosen. It configures the template, not
// the project, so it has to survive the additive path too — otherwise every
// variable is stuck at its default with no way to override it.
func TestInitAdditiveHonorsSetForTemplateVariables(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir, "--yes", "--name", "proj", "--namespace", "default"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	writeVarTemplate(t, dir)

	if _, err := runInit(t, dir, "--yes", "--template", "trainer", "--workload-name", "train",
		"--set", "workerReplicas=4"); err != nil {
		t.Fatalf("additive init with --set: %v", err)
	}

	manifest, err := os.ReadFile(filepath.Join(dir, ".okdev", "train.yaml"))
	if err != nil {
		t.Fatalf("the manifest must be scaffolded: %v", err)
	}
	if !strings.Contains(string(manifest), "replicas: 4") {
		t.Fatalf("--set must reach the rendered manifest:\n%s", manifest)
	}
}

// Project-level flags stay rejected: --set moving to the template side must not
// quietly open the rest of them.
func TestInitStillRejectsProjectFlagsAlongsideSet(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir, "--yes", "--name", "proj", "--namespace", "default"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	writeVarTemplate(t, dir)

	_, err := runInit(t, dir, "--yes", "--template", "trainer", "--workload-name", "train",
		"--set", "workerReplicas=4", "--namespace", "other")
	if err == nil || !strings.Contains(err.Error(), "namespace") {
		t.Fatalf("expected a rejection naming --namespace, got %v", err)
	}
}

// A variable with no default cannot be guessed. The additive path must say so
// by name instead of rendering an empty value into the config.
func TestPlanWorkloadAdditionNamesARequiredVariable(t *testing.T) {
	dir := t.TempDir()
	manifestDir := filepath.Join(dir, ".okdev", "templates", "manifests")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatal(err)
	}
	source := strings.Replace(varTemplateSource, "    type: int\n    default: 1\n", "    type: int\n", 1)
	if err := os.WriteFile(filepath.Join(dir, ".okdev", "templates", "trainer.yaml.tmpl"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(manifestDir, "trainer.yaml.tmpl"), []byte(varManifestSource), 0o644); err != nil {
		t.Fatal(err)
	}

	cfgPath := filepath.Join(dir, ".okdev", "okdev.yaml")
	cfg, _, err := config.LoadFromBytes([]byte(podOnlyConfig), cfgPath)
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	_, err = planWorkloadAddition(cfgPath, []byte(podOnlyConfig), cfg, config.NewTemplateVars(), nil, "train", "trainer", dir)
	if err == nil || !strings.Contains(err.Error(), "workerReplicas") {
		t.Fatalf("expected an error naming the required variable, got %v", err)
	}
}
