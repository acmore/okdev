package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteInitSTIgnoreCreatesFileFromGeneratedConfig(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, ".okdev.yaml")
	rendered := []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`)

	stignorePath, wrote, err := writeInitSTIgnore(configPath, rendered, "basic", "", false)
	if err != nil {
		t.Fatalf("writeInitSTIgnore: %v", err)
	}
	if !wrote {
		t.Fatal("expected .stignore to be written")
	}
	if stignorePath != filepath.Join(tmp, ".stignore") {
		t.Fatalf("unexpected .stignore path %q", stignorePath)
	}
	got, err := os.ReadFile(stignorePath)
	if err != nil {
		t.Fatalf("read .stignore: %v", err)
	}
	if string(got) != ".git/\n.venv/\nnode_modules/\n.DS_Store\n" {
		t.Fatalf("unexpected .stignore content %q", string(got))
	}
}

func TestWriteInitSTIgnoreDoesNotOverwriteWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, ".okdev.yaml")
	stignorePath := filepath.Join(tmp, ".stignore")
	if err := os.WriteFile(stignorePath, []byte("keep-me\n"), 0o644); err != nil {
		t.Fatalf("seed .stignore: %v", err)
	}
	rendered := []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`)

	gotPath, wrote, err := writeInitSTIgnore(configPath, rendered, "basic", "", false)
	if err != nil {
		t.Fatalf("writeInitSTIgnore: %v", err)
	}
	if wrote {
		t.Fatal("expected existing .stignore to be preserved without force")
	}
	if gotPath != stignorePath {
		t.Fatalf("unexpected returned path %q", gotPath)
	}
	got, err := os.ReadFile(stignorePath)
	if err != nil {
		t.Fatalf("read .stignore: %v", err)
	}
	if string(got) != "keep-me\n" {
		t.Fatalf("expected existing .stignore content to remain, got %q", string(got))
	}
}

func TestWriteInitSTIgnoreSkipsUnknownTemplate(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, ".okdev.yaml")
	rendered := []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`)

	stignorePath, wrote, err := writeInitSTIgnore(configPath, rendered, "./custom.yaml", "", false)
	if err != nil {
		t.Fatalf("writeInitSTIgnore: %v", err)
	}
	if wrote || stignorePath != "" {
		t.Fatalf("expected unknown template to skip .stignore generation, got path=%q wrote=%v", stignorePath, wrote)
	}
}

func TestWriteInitSTIgnoreUsesExplicitPreset(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, ".okdev.yaml")
	rendered := []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`)

	stignorePath, wrote, err := writeInitSTIgnore(configPath, rendered, "basic", "go", false)
	if err != nil {
		t.Fatalf("writeInitSTIgnore: %v", err)
	}
	if !wrote {
		t.Fatal("expected .stignore to be written")
	}
	got, err := os.ReadFile(stignorePath)
	if err != nil {
		t.Fatalf("read .stignore: %v", err)
	}
	if string(got) != ".git/\nbin/\ndist/\n.coverprofile\ncoverage.out\n.DS_Store\n" {
		t.Fatalf("unexpected .stignore content %q", string(got))
	}
}

func TestWriteInitSTIgnoreRejectsUnknownPreset(t *testing.T) {
	tmp := t.TempDir()
	configPath := filepath.Join(tmp, ".okdev.yaml")
	rendered := []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`)

	_, _, err := writeInitSTIgnore(configPath, rendered, "basic", "unknown", false)
	if err == nil {
		t.Fatal("expected unknown preset error")
	}
}

func TestDetectSTIgnorePreset(t *testing.T) {
	tmp := t.TempDir()
	if got := detectSTIgnorePreset(tmp); got != "" {
		t.Fatalf("expected no preset, got %q", got)
	}

	for _, tc := range []struct {
		name   string
		files  []string
		preset string
	}{
		{name: "go", files: []string{"go.mod"}, preset: "go"},
		{name: "node", files: []string{"package.json"}, preset: "node"},
		{name: "rust", files: []string{"Cargo.toml"}, preset: "rust"},
		{name: "python-pyproject", files: []string{"pyproject.toml"}, preset: "python"},
		{name: "python-uv", files: []string{"uv.lock"}, preset: "python"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, file := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644); err != nil {
					t.Fatalf("seed marker %s: %v", file, err)
				}
			}
			if got := detectSTIgnorePreset(dir); got != tc.preset {
				t.Fatalf("expected preset %q, got %q", tc.preset, got)
			}
		})
	}
}

func TestDetectSTIgnorePresetPriority(t *testing.T) {
	dir := t.TempDir()
	for _, file := range []string{"package.json", "pyproject.toml"} {
		if err := os.WriteFile(filepath.Join(dir, file), []byte("x"), 0o644); err != nil {
			t.Fatalf("seed marker %s: %v", file, err)
		}
	}
	if got := detectSTIgnorePreset(dir); got != "node" {
		t.Fatalf("expected node preset, got %q", got)
	}
}

func TestInitReportsDetectedSTIgnorePreset(t *testing.T) {
	tmp := t.TempDir()
	if err := os.WriteFile(filepath.Join(tmp, "go.mod"), []byte("module example.com/demo\n"), 0o644); err != nil {
		t.Fatalf("seed go.mod: %v", err)
	}

	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes"})
	cmd.SetIn(strings.NewReader(""))

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}
	if !strings.Contains(out.String(), "Using .stignore preset: go\n") {
		t.Fatalf("expected detected preset in output, got %q", out.String())
	}
}
