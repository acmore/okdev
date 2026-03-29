package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathPrefersHiddenFile(t *testing.T) {
	tmp := t.TempDir()
	hidden := filepath.Join(tmp, DefaultFile)
	legacy := filepath.Join(tmp, LegacyFile)

	writeFile(t, hidden, validConfigYAML("hidden"))
	writeFile(t, legacy, validConfigYAML("legacy"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, hidden, p)
}

func assertSameFile(t *testing.T, want, got string) {
	t.Helper()

	ws, err := os.Stat(want)
	if err != nil {
		t.Fatalf("stat wanted path: %v", err)
	}
	gs, err := os.Stat(got)
	if err != nil {
		t.Fatalf("stat got path: %v", err)
	}
	if !os.SameFile(ws, gs) {
		t.Fatalf("expected same file\nwant: %s\ngot:  %s", want, got)
	}
}

func TestResolvePathPrefersFolderConfig(t *testing.T) {
	tmp := t.TempDir()
	okdevDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(okdevDir, 0o755); err != nil {
		t.Fatal(err)
	}
	folderCfg := filepath.Join(okdevDir, "okdev.yaml")
	rootCfg := filepath.Join(tmp, DefaultFile)

	writeFile(t, folderCfg, validConfigYAML("folder"))
	writeFile(t, rootCfg, validConfigYAML("root"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, folderCfg, p)
}

func TestResolvePathFallsBackToRootConfig(t *testing.T) {
	tmp := t.TempDir()
	rootCfg := filepath.Join(tmp, DefaultFile)
	writeFile(t, rootCfg, validConfigYAML("root"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(tmp); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, rootCfg, p)
}

func TestResolvePathFindsParent(t *testing.T) {
	tmp := t.TempDir()
	child := filepath.Join(tmp, "a", "b")
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	hidden := filepath.Join(tmp, DefaultFile)
	writeFile(t, hidden, validConfigYAML("root"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}

	p, err := ResolvePath("")
	if err != nil {
		t.Fatal(err)
	}
	assertSameFile(t, hidden, p)
}

func TestLoadWithExplicitPath(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, "custom.yaml")
	writeFile(t, cfgPath, validConfigYAML("x"))

	cfg, resolved, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Metadata.Name != "x" {
		t.Fatalf("unexpected config name %q", cfg.Metadata.Name)
	}
	assertSameFile(t, cfgPath, resolved)
}

func TestLoadParsesKubeContext(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, DefaultFile)
	writeFile(t, cfgPath, "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: x\nspec:\n  namespace: default\n  kubeContext: team-staging\n")

	cfg, _, err := Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.KubeContext != "team-staging" {
		t.Fatalf("unexpected kubeContext %q", cfg.Spec.KubeContext)
	}
}

func TestLoadValidationError(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, DefaultFile)
	writeFile(t, cfgPath, "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata: {}\n")

	if _, _, err := Load(cfgPath); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestLoadParsesQuotedResourceQuantities(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, DefaultFile)
	writeFile(t, cfgPath, ""+
		"apiVersion: okdev.io/v1alpha1\n"+
		"kind: DevEnvironment\n"+
		"metadata:\n"+
		"  name: gpu-dev\n"+
		"spec:\n"+
		"  namespace: default\n"+
		"  podTemplate:\n"+
		"    spec:\n"+
		"      containers:\n"+
		"        - name: dev\n"+
		"          image: nvidia/cuda:12.4.1-devel-ubuntu22.04\n"+
		"          resources:\n"+
		"            requests:\n"+
		"              cpu: \"32\"\n"+
		"              memory: \"512Gi\"\n"+
		"              nvidia.com/gpu: \"4\"\n")

	cfg, _, err := Load(cfgPath)
	if err != nil {
		t.Fatalf("expected quoted quantities to parse, got error: %v", err)
	}
	if len(cfg.Spec.PodTemplate.Spec.Containers) != 1 {
		t.Fatalf("expected 1 container, got %d", len(cfg.Spec.PodTemplate.Spec.Containers))
	}
	req := cfg.Spec.PodTemplate.Spec.Containers[0].Resources.Requests
	cpuQty := req["cpu"]
	if got := cpuQty.String(); got != "32" {
		t.Fatalf("unexpected cpu quantity %q", got)
	}
	memQty := req["memory"]
	if got := memQty.String(); got != "512Gi" {
		t.Fatalf("unexpected memory quantity %q", got)
	}
	gpuQty := req["nvidia.com/gpu"]
	if got := gpuQty.String(); got != "4" {
		t.Fatalf("unexpected gpu quantity %q", got)
	}
}

func TestResolvePathStopsAtGitRoot(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	child := filepath.Join(repo, "a", "b")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(child, 0o755); err != nil {
		t.Fatal(err)
	}
	// Config outside the git repo should not be discovered.
	writeFile(t, filepath.Join(tmp, DefaultFile), validConfigYAML("outside"))

	oldwd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(oldwd) })
	if err := os.Chdir(child); err != nil {
		t.Fatal(err)
	}

	if _, err := ResolvePath(""); err == nil {
		t.Fatal("expected no config found because discovery should stop at git root")
	}
}

func TestRootDir(t *testing.T) {
	tmp := t.TempDir()
	if got := RootDir(filepath.Join(tmp, DefaultFile)); got != tmp {
		t.Fatalf("expected root dir %q, got %q", tmp, got)
	}
	if got := RootDir(filepath.Join(tmp, ".okdev", "okdev.yaml")); got != tmp {
		t.Fatalf("expected folder config root dir %q, got %q", tmp, got)
	}
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func validConfigYAML(name string) string {
	return "apiVersion: okdev.io/v1alpha1\n" +
		"kind: DevEnvironment\n" +
		"metadata:\n  name: " + name + "\n" +
		"spec:\n" +
		"  namespace: default\n"
}
