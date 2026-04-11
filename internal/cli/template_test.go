package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTemplateListIncludesBuiltin(t *testing.T) {
	cmd := newTemplateCmd(&Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "basic") || !strings.Contains(out.String(), "built-in") {
		t.Fatalf("expected built-in basic template in output, got:\n%s", out.String())
	}
}

func TestTemplateListIncludesProjectLocal(t *testing.T) {
	projectDir := t.TempDir()
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "team.yaml.tmpl"), []byte("---\nname: team\ndescription: Team template\n---\nok"), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newTemplateCmd(&Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list", "--project-dir", projectDir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "team") || !strings.Contains(out.String(), "project") {
		t.Fatalf("expected project template in output, got:\n%s", out.String())
	}
}

func TestTemplateListDefaultsToConfigRootFromSubdir(t *testing.T) {
	projectDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(projectDir, ".okdev.yaml"), []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`), 0o644); err != nil {
		t.Fatal(err)
	}
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmplDir, "team.yaml.tmpl"), []byte("---\ndescription: Team template\n---\nok"), 0o644); err != nil {
		t.Fatal(err)
	}
	subdir := filepath.Join(projectDir, "src", "pkg")
	if err := os.MkdirAll(subdir, 0o755); err != nil {
		t.Fatal(err)
	}

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(subdir); err != nil {
		t.Fatal(err)
	}

	cmd := newTemplateCmd(&Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"list"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "team") || !strings.Contains(out.String(), "project") {
		t.Fatalf("expected project template from config root, got:\n%s", out.String())
	}
}

func TestTemplateShowBuiltin(t *testing.T) {
	cmd := newTemplateCmd(&Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "basic"})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "Name:") || !strings.Contains(out.String(), "basic") {
		t.Fatalf("expected basic show output, got:\n%s", out.String())
	}
}

func TestTemplateShowWithVars(t *testing.T) {
	projectDir := t.TempDir()
	tmplDir := filepath.Join(projectDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	content := "---\nname: pytorch-ddp\ndescription: Distributed training\nvariables:\n  - name: numWorkers\n    description: Worker count\n    type: int\n    default: 2\n---\nok"
	if err := os.WriteFile(filepath.Join(tmplDir, "pytorch-ddp.yaml.tmpl"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	cmd := newTemplateCmd(&Options{})
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetArgs([]string{"show", "pytorch-ddp", "--project-dir", projectDir})
	if err := cmd.Execute(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), "numWorkers") || !strings.Contains(out.String(), "int") {
		t.Fatalf("expected variable details in output, got:\n%s", out.String())
	}
}
