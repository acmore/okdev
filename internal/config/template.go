package config

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/template"
	"time"
)

type templateHTTPDoer interface {
	Do(*http.Request) (*http.Response, error)
}

//go:embed templates/*.yaml.tmpl templates/manifests/*.yaml.tmpl
var embeddedTemplates embed.FS

// builtinNames maps template names to their embedded file paths.
var builtinNames = map[string]string{
	"basic": "templates/basic.yaml.tmpl",
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
	"basic": "default",
}

var templateHTTPClient templateHTTPDoer = &http.Client{Timeout: 30 * time.Second}

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
	WorkloadType     string
	ManifestPath     string
	InjectPaths      []string
	AttachContainer  string
	GenericPreset    string
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
		WorkloadType:     "pod",
		TTLHours:         72,
	}
}

// ResolveTemplate loads raw template content from a built-in name, file path, or URL.
func ResolveTemplate(ref string) (string, error) {
	return ResolveTemplateContext(context.Background(), ref)
}

// ResolveTemplateContext loads raw template content from a built-in name, file path, or URL.
func ResolveTemplateContext(ctx context.Context, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		ref = "basic"
	}
	hasPathIndicator := strings.Contains(ref, "/") ||
		strings.HasSuffix(ref, ".yaml") ||
		strings.HasSuffix(ref, ".yml") ||
		strings.HasSuffix(ref, ".tmpl")

	if hasPathIndicator {
		if content, err := os.ReadFile(ref); err == nil {
			return string(content), nil
		}
	}

	if !hasPathIndicator {
		if content, err := resolveUserTemplate(ref); err == nil {
			return content, nil
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
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", fmt.Errorf("build request for template %q: %w", url, err)
	}
	resp, err := templateHTTPClient.Do(req) //nolint:gosec // user-provided URL is intentional
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
	var names []string
	for name := range builtinNames {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func UserTemplateNames() ([]string, error) {
	dir, err := userTemplateDir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("read user template registry: %w", err)
	}
	var names []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasSuffix(name, ".yaml.tmpl") {
			continue
		}
		names = append(names, strings.TrimSuffix(name, ".yaml.tmpl"))
	}
	sort.Strings(names)
	return names, nil
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

func userTemplateDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("resolve user home for template registry: %w", err)
	}
	return filepath.Join(home, ".okdev", "templates"), nil
}

func resolveUserTemplate(name string) (string, error) {
	dir, err := userTemplateDir()
	if err != nil {
		return "", err
	}
	path := filepath.Join(dir, name+".yaml.tmpl")
	rel, err := filepath.Rel(dir, path)
	if err != nil || strings.HasPrefix(rel, "..") {
		return "", fmt.Errorf("template name %q resolves outside registry", name)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	return string(content), nil
}

func RenderEmbeddedTemplate(path string, vars *TemplateVars) (string, error) {
	raw, err := embeddedTemplates.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read embedded template %q: %w", path, err)
	}
	tmpl, err := template.New(filepath.Base(path)).Parse(string(raw))
	if err != nil {
		return "", fmt.Errorf("parse embedded template %q: %w", path, err)
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("render embedded template %q: %w", path, err)
	}
	return buf.String(), nil
}
