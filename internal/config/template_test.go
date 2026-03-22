package config

import (
	"strings"
	"testing"
)

func TestDefaultTemplateVars(t *testing.T) {
	vars := NewTemplateVars()
	if vars.Namespace != "default" {
		t.Fatalf("expected default namespace, got %q", vars.Namespace)
	}
	if vars.SSHUser != "root" {
		t.Fatalf("expected root ssh user, got %q", vars.SSHUser)
	}
	if vars.SyncLocal != "." {
		t.Fatalf("expected . sync local, got %q", vars.SyncLocal)
	}
	if vars.SyncRemote != "/workspace" {
		t.Fatalf("expected /workspace sync remote, got %q", vars.SyncRemote)
	}
}

func TestRenderBuiltinBasic(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "test-project"
	out, err := RenderTemplate("basic", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: test-project") {
		t.Fatal("expected rendered name in output")
	}
	if !strings.Contains(out, "namespace: default") {
		t.Fatal("expected namespace in output")
	}
	// Verify runtime template syntax is preserved literally
	if !strings.Contains(out, "{{ .Repo }}-{{ .Branch }}-{{ .User }}") {
		t.Fatal("expected session name template to be preserved literally")
	}
}

func TestResolveTemplateBuiltinName(t *testing.T) {
	content, err := ResolveTemplate("basic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "{{ .Name }}") {
		t.Fatal("expected template variable in raw content")
	}
}

func TestResolveTemplateFilePath(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := tmp + "/custom.yaml.tmpl"
	writeFile(t, tmplPath, "name: {{ .Name }}")
	content, err := ResolveTemplate(tmplPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "name: {{ .Name }}" {
		t.Fatalf("unexpected content: %q", content)
	}
}
