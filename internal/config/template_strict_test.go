package config

import (
	"strings"
	"testing"
)

// Referencing a variable the template never declared is always a mistake in the
// template, but Go renders it as the literal "<no value>". For a string that
// silently wrote a broken config; for anything compared numerically it surfaced
// as "invalid type for comparison", which names neither the variable nor the
// fix.
func TestRenderTemplateContentRejectsAnUndeclaredVariable(t *testing.T) {
	_, err := RenderTemplateContent("okdev", "image: {{ .Vars.trainImage }}\n", NewTemplateVars(), map[string]any{})
	if err == nil {
		t.Fatal("a template referencing an undeclared variable must not render silently")
	}
	if !strings.Contains(err.Error(), "trainImage") {
		t.Fatalf("the error must name the undeclared variable, got: %v", err)
	}
	if !strings.Contains(err.Error(), "variables:") {
		t.Fatalf("the error must point at the frontmatter fix, got: %v", err)
	}
}

// The comparison case is the one that produced the unreadable Go error.
func TestRenderTemplateContentRejectsAnUndeclaredVariableInAComparison(t *testing.T) {
	_, err := RenderTemplateContent("okdev", "{{ if gt .Vars.workerReplicas 0 }}x{{ end }}", NewTemplateVars(), map[string]any{})
	if err == nil {
		t.Fatal("comparing against an undeclared variable must be an error")
	}
	if !strings.Contains(err.Error(), "workerReplicas") {
		t.Fatalf("the error must name the undeclared variable, got: %v", err)
	}
	if strings.Contains(err.Error(), "invalid type for comparison") {
		t.Fatalf("the raw Go template error must not be what the user sees, got: %v", err)
	}
}

// Declared variables must keep rendering exactly as before — the strictness is
// only about names nothing ever supplied.
func TestRenderTemplateContentRendersDeclaredVariables(t *testing.T) {
	out, err := RenderTemplateContent("okdev",
		"replicas: {{ if gt .Vars.workerReplicas 0 }}{{ .Vars.workerReplicas }}{{ else }}1{{ end }}\nimage: {{ .Vars.image }}\n",
		NewTemplateVars(),
		map[string]any{"workerReplicas": 4, "image": "ubuntu:22.04"})
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "replicas: 4") || !strings.Contains(out, "image: ubuntu:22.04") {
		t.Fatalf("declared variables must render:\n%s", out)
	}
}

// A template that uses no variables at all must stay unaffected.
func TestRenderTemplateContentWithoutVariablesIsUnaffected(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "demo"
	out, err := RenderTemplateContent("okdev", "name: {{ .Name }}\nns: {{ .Namespace }}\n", vars, nil)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !strings.Contains(out, "name: demo") || !strings.Contains(out, "ns: default") {
		t.Fatalf("built-in fields must render:\n%s", out)
	}
}
