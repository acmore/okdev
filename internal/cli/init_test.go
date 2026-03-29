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

func TestInitUsesFolderConfigWhenScaffoldingWorkload(t *testing.T) {
	tmp := t.TempDir()
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd(&Options{})
	cmd.SetArgs([]string{"--yes", "--workload", "job"})
	cmd.SetIn(strings.NewReader(""))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmp, ".okdev", "okdev.yaml")); err != nil {
		t.Fatalf("expected folder config, got err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".okdev.yaml")); !os.IsNotExist(err) {
		t.Fatalf("expected root config to be absent, err=%v", err)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".stignore")); err != nil {
		t.Fatalf("expected root .stignore, got err=%v", err)
	}
}

func TestInitUsesRootConfigForPod(t *testing.T) {
	tmp := t.TempDir()
	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	cmd := newInitCmd(&Options{})
	cmd.SetArgs([]string{"--yes"})
	cmd.SetIn(strings.NewReader(""))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	if _, err := os.Stat(filepath.Join(tmp, ".okdev.yaml")); err != nil {
		t.Fatalf("expected root config, got err=%v", err)
	}
}

func TestInitScaffoldsJobManifest(t *testing.T) {
	tmp := t.TempDir()
	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "job", "--sync-remote", "/train"})
	cmd.SetIn(strings.NewReader(""))

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	manifestPath := filepath.Join(tmp, ".okdev", "job.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read job manifest: %v", err)
	}
	if !strings.Contains(string(raw), "kind: Job") {
		t.Fatalf("expected job scaffold, got %q", string(raw))
	}
	if !strings.Contains(string(raw), "mountPath: /train") {
		t.Fatalf("expected job scaffold to use sync remote mount path, got %q", string(raw))
	}
	cfgRaw, err := os.ReadFile(filepath.Join(tmp, ".okdev.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(cfgRaw), "type: job") {
		t.Fatalf("expected job workload block, got %q", string(cfgRaw))
	}
	if !strings.Contains(string(cfgRaw), "manifestPath: .okdev/job.yaml") {
		t.Fatalf("expected job manifest path, got %q", string(cfgRaw))
	}
}

func TestInitScaffoldsGenericDeploymentPreset(t *testing.T) {
	tmp := t.TempDir()
	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "generic", "--generic-preset", "deployment", "--sync-remote", "/train"})
	cmd.SetIn(strings.NewReader(""))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	manifestPath := filepath.Join(tmp, ".okdev", "deployment.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read deployment manifest: %v", err)
	}
	if !strings.Contains(string(raw), "kind: Deployment") {
		t.Fatalf("expected deployment scaffold, got %q", string(raw))
	}
	if !strings.Contains(string(raw), "mountPath: /train") {
		t.Fatalf("expected deployment scaffold to use sync remote mount path, got %q", string(raw))
	}
	cfgRaw, err := os.ReadFile(filepath.Join(tmp, ".okdev.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(cfgRaw), "type: generic") {
		t.Fatalf("expected generic workload block, got %q", string(cfgRaw))
	}
	if !strings.Contains(string(cfgRaw), "manifestPath: .okdev/deployment.yaml") {
		t.Fatalf("expected deployment manifest path, got %q", string(cfgRaw))
	}
}

func TestInitRejectsGenericWithoutManifestPath(t *testing.T) {
	tmp := t.TempDir()
	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "generic", "--inject-path", "spec.template"})
	cmd.SetIn(strings.NewReader(""))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected generic manifest path error")
	}
	if !strings.Contains(err.Error(), "--manifest-path is required") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestInitScaffoldsPyTorchJobManifest(t *testing.T) {
	tmp := t.TempDir()
	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "pytorchjob", "--sync-remote", "/train"})
	cmd.SetIn(strings.NewReader(""))

	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	manifestPath := filepath.Join(tmp, ".okdev", "pytorchjob.yaml")
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatalf("read pytorchjob manifest: %v", err)
	}
	if !strings.Contains(string(raw), "kind: PyTorchJob") {
		t.Fatalf("expected pytorchjob scaffold, got %q", string(raw))
	}
	if !strings.Contains(string(raw), "name: dev") {
		t.Fatalf("expected dev container in manifest, got %q", string(raw))
	}
	if !strings.Contains(string(raw), "mountPath: /train") {
		t.Fatalf("expected pytorchjob scaffold to use sync remote mount path, got %q", string(raw))
	}
	cfgRaw, err := os.ReadFile(filepath.Join(tmp, ".okdev.yaml"))
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if !strings.Contains(string(cfgRaw), "type: pytorchjob") {
		t.Fatalf("expected pytorchjob workload block, got %q", string(cfgRaw))
	}
}

func TestInitManifestPreservedWithoutForce(t *testing.T) {
	tmp := t.TempDir()
	okdevDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(okdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(okdevDir, "job.yaml"), []byte("keep-me"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "job"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(okdevDir, "job.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(data) != "keep-me" {
		t.Fatal("expected existing manifest to be preserved without --force")
	}
}

func TestInitManifestOverwrittenWithForce(t *testing.T) {
	tmp := t.TempDir()
	okdevDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(okdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(okdevDir, "job.yaml"), []byte("keep-me"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "job", "--force"})
	cmd.SetIn(strings.NewReader(""))
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("init execute: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(okdevDir, "job.yaml"))
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if string(data) == "keep-me" {
		t.Fatal("expected manifest to be overwritten with --force")
	}
	if !strings.Contains(string(data), "kind: Job") {
		t.Fatalf("expected scaffolded job manifest, got %q", string(data))
	}
}

func TestInitRejectsUnknownGenericPreset(t *testing.T) {
	tmp := t.TempDir()
	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--workload", "generic", "--generic-preset", "foo", "--manifest-path", "m.yaml", "--inject-path", "spec.template"})
	cmd.SetIn(strings.NewReader(""))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected unknown generic preset error")
	}
	if !strings.Contains(err.Error(), "unknown --generic-preset") {
		t.Fatalf("unexpected error %v", err)
	}
}

func TestInitRejectsCustomTemplateWithoutRenderedWorkload(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "custom.yaml.tmpl")
	if err := os.WriteFile(tmplPath, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	opts := &Options{ConfigPath: filepath.Join(tmp, ".okdev.yaml")}
	cmd := newInitCmd(opts)
	cmd.SetArgs([]string{"--yes", "--template", tmplPath, "--workload", "job"})
	cmd.SetIn(strings.NewReader(""))

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected custom workload validation error")
	}
	if !strings.Contains(err.Error(), "custom template must render spec.workload") {
		t.Fatalf("unexpected error %v", err)
	}
}
