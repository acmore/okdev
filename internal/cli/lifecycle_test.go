package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func testLifecycleCfg() *config.DevEnvironment {
	return &config.DevEnvironment{
		Spec: config.DevEnvSpec{
			Volumes: nil,
		},
	}
}

func TestResolvePostCreateCommandPrefersExplicit(t *testing.T) {
	cfg := testLifecycleCfg()
	cfg.Spec.Lifecycle.PostCreate = "make setup"
	got := resolvePostCreateCommand(cfg, "/tmp/.okdev.yaml")
	if got != "make setup" {
		t.Fatalf("unexpected command: %q", got)
	}
}

func TestResolvePreStopCommandPrefersExplicit(t *testing.T) {
	cfg := testLifecycleCfg()
	cfg.Spec.Lifecycle.PreStop = "make clean"
	got := resolvePreStopCommand(cfg, "/tmp/.okdev.yaml")
	if got != "make clean" {
		t.Fatalf("unexpected command: %q", got)
	}
}

func TestResolveLifecycleScriptsFromConfigRoot(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".okdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".okdev", "post-create.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".okdev", "pre-stop.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testLifecycleCfg()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if got := resolvePostCreateCommand(cfg, cfgPath); got != "/workspace/.okdev/post-create.sh" {
		t.Fatalf("unexpected postCreate: %q", got)
	}
	if got := resolvePreStopCommand(cfg, cfgPath); got != "/workspace/.okdev/pre-stop.sh" {
		t.Fatalf("unexpected preStop: %q", got)
	}
}

func TestResolvePostSyncCommandPrefersExplicit(t *testing.T) {
	cfg := testLifecycleCfg()
	cfg.Spec.Lifecycle.PostSync = "pip install -e ."
	got := resolvePostSyncCommand(cfg, "/tmp/.okdev.yaml")
	if got != "pip install -e ." {
		t.Fatalf("unexpected command: %q", got)
	}
}

func TestResolvePostSyncCommandFallsBackToScript(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".okdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".okdev", "post-sync.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testLifecycleCfg()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if got := resolvePostSyncCommand(cfg, cfgPath); got != "/workspace/.okdev/post-sync.sh" {
		t.Fatalf("unexpected postSync: %q", got)
	}
}

func TestResolvePostSyncCommandReturnsEmptyWhenNotConfigured(t *testing.T) {
	cfg := testLifecycleCfg()
	got := resolvePostSyncCommand(cfg, "/tmp/.okdev.yaml")
	if got != "" {
		t.Fatalf("expected empty, got: %q", got)
	}
}

func TestResolveLifecycleScriptsFromFolderConfig(t *testing.T) {
	tmp := t.TempDir()
	if err := os.MkdirAll(filepath.Join(tmp, ".okdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, ".okdev", "post-create.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := testLifecycleCfg()
	cfgPath := filepath.Join(tmp, ".okdev", "okdev.yaml")
	if got := resolvePostCreateCommand(cfg, cfgPath); got != "/workspace/.okdev/post-create.sh" {
		t.Fatalf("unexpected postCreate: %q", got)
	}
}
