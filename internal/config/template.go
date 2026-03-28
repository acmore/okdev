package config

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"
)

//go:embed templates/*.yaml.tmpl
var embeddedTemplates embed.FS

// builtinNames maps template names to their embedded file paths.
var builtinNames = map[string]string{
	"basic":           "templates/basic.yaml.tmpl",
	"gpu":             "templates/gpu.yaml.tmpl",
	"llm-gpu":         "templates/gpu.yaml.tmpl",
	"llm-stack":       "templates/llm-stack.yaml.tmpl",
	"multi-container": "templates/llm-stack.yaml.tmpl",
}

var stignorePresets = map[string][]string{
	"default": {
		".git/",
		".venv/",
		"node_modules/",
		".DS_Store",
	},
	"python": {
		".git/",
		".venv/",
		"__pycache__/",
		".pytest_cache/",
		".mypy_cache/",
		".ruff_cache/",
		".DS_Store",
	},
	"node": {
		".git/",
		"node_modules/",
		".next/",
		"dist/",
		"coverage/",
		".DS_Store",
	},
	"go": {
		".git/",
		"bin/",
		"dist/",
		".coverprofile",
		"coverage.out",
		".DS_Store",
	},
	"rust": {
		".git/",
		"target/",
		".DS_Store",
	},
}

var builtinTemplateStignorePreset = map[string]string{
	"basic":           "default",
	"gpu":             "default",
	"llm-gpu":         "default",
	"llm-stack":       "default",
	"multi-container": "default",
}

// TemplateVars holds all variables available to templates.
type TemplateVars struct {
	Name             string
	Namespace        string
	SidecarImage     string
	SyncthingVersion string
	SyncLocal        string
	SyncRemote       string
	SSHUser          string
	Ports            []PortVar
	BaseImage        string
	GPUCount         string
	TTLHours         int
}

type PortVar struct {
	Name   string
	Local  int
	Remote int
}

// NewTemplateVars returns TemplateVars with sensible defaults.
func NewTemplateVars() *TemplateVars {
	return &TemplateVars{
		Name:             "",
		Namespace:        "default",
		SidecarImage:     DefaultSidecarImage,
		SyncthingVersion: DefaultSyncthingVersion,
		SyncLocal:        ".",
		SyncRemote:       "/workspace",
		SSHUser:          "root",
		BaseImage:        "nvidia/cuda:12.4.1-devel-ubuntu22.04",
		GPUCount:         "1",
		TTLHours:         72,
	}
}

// ResolveTemplate loads raw template content from a built-in name, file path, or URL.
func ResolveTemplate(ref string) (string, error) {
	return ResolveTemplateContext(context.Background(), ref)
}

// ResolveTemplateContext loads raw template content from a built-in name, file path, or URL.
func ResolveTemplateContext(ctx context.Context, ref string) (string, error) {
	hasPathIndicator := strings.Contains(ref, "/") ||
		strings.HasSuffix(ref, ".yaml") ||
		strings.HasSuffix(ref, ".yml") ||
		strings.HasSuffix(ref, ".tmpl")

	if hasPathIndicator {
		if content, err := os.ReadFile(ref); err == nil {
			return string(content), nil
		}
	}

	// Check built-in
	if path, ok := builtinNames[ref]; ok {
		content, err := embeddedTemplates.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read embedded template %q: %w", ref, err)
		}
		return string(content), nil
	}

	if !hasPathIndicator {
		// Try as file anyway
		if content, err := os.ReadFile(ref); err == nil {
			return string(content), nil
		}
	}

	// Treat as URL
	return fetchTemplateURL(ctx, ref)
}

func fetchTemplateURL(ctx context.Context, url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for template %q: %w", url, err)
	}
	resp, err := client.Do(req) //nolint:gosec // user-provided URL is intentional
	if err != nil {
		return "", fmt.Errorf("fetch template from %q: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch template from %q: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return "", fmt.Errorf("read template from %q: %w", url, err)
	}
	return string(body), nil
}

// RenderTemplate resolves a template by ref and renders it with the given vars.
func RenderTemplate(ref string, vars *TemplateVars) (string, error) {
	return RenderTemplateContext(context.Background(), ref, vars)
}

// RenderTemplateContext resolves a template by ref and renders it with the given vars.
func RenderTemplateContext(ctx context.Context, ref string, vars *TemplateVars) (string, error) {
	raw, err := ResolveTemplateContext(ctx, ref)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("okdev").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}

// BuiltinTemplateNames returns the list of available built-in template names.
func BuiltinTemplateNames() []string {
	seen := map[string]bool{}
	var names []string
	for name := range builtinNames {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

func BuiltinTemplateLocalIgnores(ref string) []string {
	preset, ok := builtinTemplateStignorePreset[ref]
	if !ok {
		return nil
	}
	return STIgnorePreset(preset)
}

func STIgnorePreset(name string) []string {
	patterns, ok := stignorePresets[name]
	if !ok {
		return nil
	}
	out := make([]string, len(patterns))
	copy(out, patterns)
	return out
}
