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
	if strings.Contains(out, "\n    exclude:\n") {
		t.Fatalf("expected starter template to keep local ignore rules out of .okdev.yaml, got:\n%s", out)
	}
}

func TestBuiltinTemplateLocalIgnores(t *testing.T) {
	patterns := BuiltinTemplateLocalIgnores("basic")
	if len(patterns) == 0 {
		t.Fatal("expected built-in local ignore patterns")
	}
	if patterns[0] != ".git/" {
		t.Fatalf("unexpected first pattern %q", patterns[0])
	}
	if got := BuiltinTemplateLocalIgnores("./custom.yaml"); got != nil {
		t.Fatalf("expected nil ignores for custom template, got %+v", got)
	}
}

func TestSTIgnorePreset(t *testing.T) {
	patterns := STIgnorePreset("go")
	if len(patterns) == 0 {
		t.Fatal("expected go preset patterns")
	}
	if patterns[1] != "bin/" {
		t.Fatalf("unexpected go preset pattern %q", patterns[1])
	}
	if got := STIgnorePreset("missing"); got != nil {
		t.Fatalf("expected nil for unknown preset, got %+v", got)
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
