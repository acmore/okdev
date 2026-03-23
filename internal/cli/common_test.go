package cli

import (
	"bytes"
	"os"
	"strings"
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

func TestTransientStatusRendersAndClears(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "Loading config /tmp/.okdev.yaml",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go status.run(5 * time.Millisecond)
	time.Sleep(12 * time.Millisecond)
	status.stop()

	got := out.String()
	if !strings.Contains(got, "Loading config /tmp/.okdev.yaml") {
		t.Fatalf("expected status message in output, got %q", got)
	}
	if !strings.Contains(got, "\r\033[K") {
		t.Fatalf("expected clear-line sequence in output, got %q", got)
	}
}

func TestTransientStatusUpdate(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "Loading config /tmp/.okdev.yaml",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
	}

	go status.run(5 * time.Millisecond)
	time.Sleep(8 * time.Millisecond)
	status.update("Installing tmux via apt-get")
	time.Sleep(8 * time.Millisecond)
	status.stop()

	got := out.String()
	if !strings.Contains(got, "Installing tmux via apt-get") {
		t.Fatalf("expected updated status message in output, got %q", got)
	}
}

func TestAnnounceConfigPathFallsBackToStaticOutput(t *testing.T) {
	var out bytes.Buffer
	done := announceConfigPathWithWriter(&out, "/tmp/.okdev.yaml", false)
	done(true)

	if got := out.String(); got != "Using config: /tmp/.okdev.yaml\n" {
		t.Fatalf("unexpected static output %q", got)
	}
}
