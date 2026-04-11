package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMigrateWithTemplatePreservesUserValues(t *testing.T) {
	projectDir := t.TempDir()
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmplContent := `---
name: team
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: template-ns
  ssh:
    user: dev
  sync:
    paths:
      - ".:/workspace"
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	if err := os.WriteFile(filepath.Join(tmplDir, "team.yaml.tmpl"), []byte(tmplContent), 0o644); err != nil {
		t.Fatal(err)
	}

	existingConfig := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: my-custom-ns
  ssh:
    user: root
  sync:
    paths:
      - ".:/workspace"
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	cfgPath := filepath.Join(projectDir, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(existingConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := mergeTemplateConfig(cfgPath, "team", nil, nil, projectDir, true)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"my-custom-ns", "user: root", "template:", "name: team"} {
		if !strings.Contains(result.merged, want) {
			t.Fatalf("expected merged config to contain %q, got:\n%s", want, result.merged)
		}
	}
	if !strings.Contains(result.summary, "preserved") {
		t.Fatalf("expected preserved summary, got %q", result.summary)
	}
}

func TestMigrateWithTemplateUsesStoredVarsAndSetOverrides(t *testing.T) {
	projectDir := t.TempDir()
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmplContent := `---
name: team
variables:
  - name: baseImage
    type: string
    default: ubuntu:22.04
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: default
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .Vars.baseImage }}
`
	if err := os.WriteFile(filepath.Join(tmplDir, "team.yaml.tmpl"), []byte(tmplContent), 0o644); err != nil {
		t.Fatal(err)
	}
	existingConfig := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  template:
    name: team
    vars:
      baseImage: debian:12
  namespace: default
`
	cfgPath := filepath.Join(projectDir, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(existingConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := mergeTemplateConfig(cfgPath, "team", []string{"baseImage=alpine:3.20"}, nil, projectDir, true)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(result.merged, "baseImage: alpine:3.20") {
		t.Fatalf("expected --set var persisted, got:\n%s", result.merged)
	}
}

func TestMigrateWithTemplatePromptsForMissingRequiredVars(t *testing.T) {
	projectDir := t.TempDir()
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tmplContent := `---
name: team
variables:
  - name: baseImage
    description: Base image
    type: string
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: default
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .Vars.baseImage }}
`
	if err := os.WriteFile(filepath.Join(tmplDir, "team.yaml.tmpl"), []byte(tmplContent), 0o644); err != nil {
		t.Fatal(err)
	}
	existingConfig := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec:
  namespace: default
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	cfgPath := filepath.Join(projectDir, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(existingConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	result, err := mergeTemplateConfigWithPrompt(cfgPath, "team", nil, nil, projectDir, false, true, strings.NewReader("debian:12\n"), &out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Base image (baseImage)") {
		t.Fatalf("expected prompt for required variable, got %q", out.String())
	}
	if !strings.Contains(result.merged, "baseImage: debian:12") || !strings.Contains(result.merged, "image: debian:12") {
		t.Fatalf("expected prompted value in merged config, got:\n%s", result.merged)
	}
}
