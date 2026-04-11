package config

import "testing"

func TestParseFrontmatter(t *testing.T) {
	raw := "---\nname: pytorch-ddp\ndescription: Distributed training\nvariables:\n  - name: numWorkers\n    description: Worker count\n    type: int\n    default: 2\n  - name: baseImage\n    description: Container image\n    type: string\n    default: pytorch:latest\n---\napiVersion: okdev.dev/v1\nname: {{ .Name }}"

	meta, body, err := ParseFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "pytorch-ddp" {
		t.Fatalf("expected name pytorch-ddp, got %q", meta.Name)
	}
	if meta.Description != "Distributed training" {
		t.Fatalf("expected description, got %q", meta.Description)
	}
	if len(meta.Variables) != 2 {
		t.Fatalf("expected 2 variables, got %d", len(meta.Variables))
	}
	if meta.Variables[0].Name != "numWorkers" || meta.Variables[0].Type != "int" {
		t.Fatalf("unexpected first variable: %+v", meta.Variables[0])
	}
	if meta.Variables[0].Default != 2 {
		t.Fatalf("expected default 2, got %v", meta.Variables[0].Default)
	}
	if body != "apiVersion: okdev.dev/v1\nname: {{ .Name }}" {
		t.Fatalf("unexpected body: %q", body)
	}
}

func TestParseFrontmatterNoFrontmatter(t *testing.T) {
	raw := "apiVersion: okdev.dev/v1\nname: {{ .Name }}"
	meta, body, err := ParseFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "" || len(meta.Variables) != 0 {
		t.Fatalf("expected empty meta, got %+v", meta)
	}
	if body != raw {
		t.Fatalf("expected unmodified body, got %q", body)
	}
}

func TestCoerceVariableValue(t *testing.T) {
	tests := []struct {
		name     string
		varDef   TemplateVariable
		input    string
		expected any
		wantErr  bool
	}{
		{"string value", TemplateVariable{Name: "img", Type: "string"}, "ubuntu:22.04", "ubuntu:22.04", false},
		{"int value", TemplateVariable{Name: "count", Type: "int"}, "4", 4, false},
		{"bool true", TemplateVariable{Name: "flag", Type: "bool"}, "true", true, false},
		{"bool false", TemplateVariable{Name: "flag", Type: "bool"}, "false", false, false},
		{"invalid int", TemplateVariable{Name: "count", Type: "int"}, "abc", nil, true},
		{"invalid bool", TemplateVariable{Name: "flag", Type: "bool"}, "maybe", nil, true},
		{"empty string type", TemplateVariable{Name: "x", Type: ""}, "hello", "hello", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := CoerceVariableValue(tt.varDef, tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got != tt.expected {
				t.Fatalf("expected %v (%T), got %v (%T)", tt.expected, tt.expected, got, got)
			}
		})
	}
}

func TestResolveVariables(t *testing.T) {
	meta := &TemplateMeta{
		Variables: []TemplateVariable{
			{Name: "numWorkers", Type: "int", Default: 2},
			{Name: "baseImage", Type: "string", Default: "pytorch:latest"},
			{Name: "enableGPU", Type: "bool"},
		},
	}
	got, err := ResolveVariables(meta, map[string]string{"numWorkers": "8", "enableGPU": "true"}, map[string]any{"baseImage": "custom:v1"})
	if err != nil {
		t.Fatal(err)
	}
	if got["numWorkers"] != 8 || got["baseImage"] != "custom:v1" || got["enableGPU"] != true {
		t.Fatalf("unexpected resolved vars: %+v", got)
	}
}

func TestResolveVariablesMissingRequired(t *testing.T) {
	meta := &TemplateMeta{Variables: []TemplateVariable{{Name: "required_var", Type: "string"}}}
	if _, err := ResolveVariables(meta, nil, nil); err == nil {
		t.Fatal("expected error for missing required variable")
	}
}
