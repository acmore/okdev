package cli

import (
	"os"
	"path/filepath"
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

	stignorePath, wrote, err := writeInitSTIgnore(configPath, rendered, "basic", false)
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
	if string(got) != ".git/\n.venv/\nnode_modules/\n" {
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

	gotPath, wrote, err := writeInitSTIgnore(configPath, rendered, "basic", false)
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

	stignorePath, wrote, err := writeInitSTIgnore(configPath, rendered, "./custom.yaml", false)
	if err != nil {
		t.Fatalf("writeInitSTIgnore: %v", err)
	}
	if wrote || stignorePath != "" {
		t.Fatalf("expected unknown template to skip .stignore generation, got path=%q wrote=%v", stignorePath, wrote)
	}
}
