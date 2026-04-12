package config

import (
	"fmt"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

// TemplateMeta holds optional frontmatter metadata from a template file.
type TemplateMeta struct {
	Name        string             `yaml:"name"`
	Description string             `yaml:"description"`
	Variables   []TemplateVariable `yaml:"variables"`
	Files       []TemplateFile     `yaml:"files"`
}

// TemplateVariable declares a custom variable that a template accepts.
type TemplateVariable struct {
	Name        string `yaml:"name"`
	Description string `yaml:"description"`
	Type        string `yaml:"type"`
	Default     any    `yaml:"default,omitempty"`
}

// TemplateFile declares an additional file rendered by okdev init.
type TemplateFile struct {
	Path     string `yaml:"path"`
	Template string `yaml:"template"`
}

// HasDefault returns true when a default value was declared.
func (v TemplateVariable) HasDefault() bool {
	return v.Default != nil
}

// ParseFrontmatter extracts YAML frontmatter from a template string.
func ParseFrontmatter(raw string) (*TemplateMeta, string, error) {
	if !strings.HasPrefix(raw, "---\n") {
		return &TemplateMeta{}, raw, nil
	}
	end := strings.Index(raw[4:], "\n---")
	if end < 0 {
		return &TemplateMeta{}, raw, nil
	}
	frontmatter := raw[4 : 4+end]
	body := raw[4+end+4:]
	body = strings.TrimPrefix(body, "\n")

	var meta TemplateMeta
	if err := yaml.Unmarshal([]byte(frontmatter), &meta); err != nil {
		return nil, "", fmt.Errorf("parse template frontmatter: %w", err)
	}
	return &meta, body, nil
}

// CoerceVariableValue converts a string value to the declared variable type.
func CoerceVariableValue(v TemplateVariable, raw string) (any, error) {
	typ := strings.TrimSpace(v.Type)
	if typ == "" {
		typ = "string"
	}
	switch typ {
	case "string":
		return raw, nil
	case "int":
		n, err := strconv.Atoi(raw)
		if err != nil {
			return nil, fmt.Errorf("variable %q: expected int, got %q", v.Name, raw)
		}
		return n, nil
	case "bool":
		b, err := strconv.ParseBool(raw)
		if err != nil {
			return nil, fmt.Errorf("variable %q: expected bool, got %q", v.Name, raw)
		}
		return b, nil
	default:
		return nil, fmt.Errorf("variable %q: unsupported type %q", v.Name, typ)
	}
}

// ResolveVariables resolves template variables from flags, stored values, and defaults.
func ResolveVariables(meta *TemplateMeta, sets map[string]string, stored map[string]any) (map[string]any, error) {
	if meta == nil {
		return map[string]any{}, nil
	}
	result := make(map[string]any, len(meta.Variables))
	for _, v := range meta.Variables {
		if raw, ok := sets[v.Name]; ok {
			coerced, err := CoerceVariableValue(v, raw)
			if err != nil {
				return nil, err
			}
			result[v.Name] = coerced
			continue
		}
		if val, ok := stored[v.Name]; ok {
			result[v.Name] = val
			continue
		}
		if v.HasDefault() {
			result[v.Name] = v.Default
			continue
		}
		return nil, fmt.Errorf("variable %q is required (no default) and no value provided", v.Name)
	}
	return result, nil
}
