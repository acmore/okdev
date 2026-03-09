package cli

import (
	"os"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
)

func TestNamesAndLabels(t *testing.T) {
	t.Setenv("USER", "alice")
	cfg := &config.DevEnvironment{Metadata: config.Metadata{Name: "proj"}}
	labels := labelsForSession(&Options{}, cfg, "sess1")
	if labels["okdev.io/session"] != "sess1" {
		t.Fatalf("session label mismatch: %+v", labels)
	}
	if labels["okdev.io/owner"] != "alice" {
		t.Fatalf("owner label mismatch: %+v", labels)
	}
	if labels["okdev.io/shareable"] != "false" {
		t.Fatalf("shareable label mismatch: %+v", labels)
	}
	if podName("sess1") != "okdev-sess1" {
		t.Fatalf("unexpected pod name %q", podName("sess1"))
	}
}

func TestPVCName(t *testing.T) {
	cfg := &config.DevEnvironment{}
	if usesWorkspacePVC(cfg) {
		t.Fatal("expected pvc disabled when workspace.pvc is not configured")
	}
	cfg.Spec.Workspace.PVC.Size = "50Gi"
	if !usesWorkspacePVC(cfg) {
		t.Fatal("expected pvc enabled when size is configured")
	}
	if pvcName(cfg, "s1") != "okdev-s1-workspace" {
		t.Fatal("unexpected managed pvc name")
	}
	cfg.Spec.Workspace.PVC.Size = ""
	cfg.Spec.Workspace.PVC.ClaimName = "existing"
	if !usesWorkspacePVC(cfg) {
		t.Fatal("expected pvc enabled for explicit claim")
	}
	if pvcName(cfg, "s1") != "existing" {
		t.Fatal("expected explicit claim name")
	}
}

func TestAnnotationsForSession(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.TTLHours = 72
	cfg.Spec.Session.IdleTimeoutMinutes = 120
	a := annotationsForSession(cfg)
	if a["okdev.io/ttl-hours"] != "72" || a["okdev.io/idle-timeout-minutes"] != "120" {
		t.Fatalf("unexpected annotations: %+v", a)
	}
	if _, err := time.Parse(time.RFC3339, a["okdev.io/last-attach"]); err != nil {
		t.Fatalf("invalid timestamp: %v", err)
	}
}

func TestAge(t *testing.T) {
	if age(time.Time{}) != "-" {
		t.Fatal("zero time should render as -")
	}
	now := time.Now()
	if got := age(now.Add(-30 * time.Second)); got == "-" {
		t.Fatalf("unexpected age: %s", got)
	}
}

func TestEnsureCommandMissing(t *testing.T) {
	oldPath := os.Getenv("PATH")
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })
	_ = os.Setenv("PATH", "")
	if err := ensureCommand("definitely-not-a-real-command"); err == nil {
		t.Fatal("expected missing command error")
	}
}

func TestApplyConfigKubeContextFromConfigWhenFlagUnset(t *testing.T) {
	opts := &Options{}
	cfg := &config.DevEnvironment{}
	cfg.Spec.KubeContext = "dev-cluster"

	applyConfigKubeContext(opts, cfg)

	if opts.Context != "dev-cluster" {
		t.Fatalf("expected context from config, got %q", opts.Context)
	}
}

func TestApplyConfigKubeContextFlagWins(t *testing.T) {
	opts := &Options{Context: "flag-context"}
	cfg := &config.DevEnvironment{}
	cfg.Spec.KubeContext = "config-context"

	applyConfigKubeContext(opts, cfg)

	if opts.Context != "flag-context" {
		t.Fatalf("expected flag context to win, got %q", opts.Context)
	}
}
