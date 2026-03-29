package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
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
	if _, ok := a["okdev.io/last-attach"]; ok {
		t.Fatal("last-attach should not be in manifest annotations (set via AnnotatePod instead)")
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
		started: time.Now(),
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
		started: time.Now(),
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

func TestTransientStatusStopIsIdempotent(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "Loading config /tmp/.okdev.yaml",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		started: time.Now(),
	}

	go status.run(5 * time.Millisecond)
	time.Sleep(8 * time.Millisecond)
	status.stop()
	status.stop()
}

func TestTransientStatusShowsElapsedAfterThreshold(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "Pulling image",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		started: time.Now().Add(-5 * time.Second), // pretend started 5s ago
	}

	go status.run(5 * time.Millisecond)
	time.Sleep(12 * time.Millisecond)
	status.stop()

	got := out.String()
	if !strings.Contains(got, "(5s)") && !strings.Contains(got, "(6s)") {
		t.Fatalf("expected elapsed time in output after threshold, got %q", got)
	}
}

func TestTransientStatusHidesElapsedBelowThreshold(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "Quick check",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		started: time.Now(), // just started
	}

	go status.run(5 * time.Millisecond)
	time.Sleep(12 * time.Millisecond)
	status.stop()

	got := out.String()
	if strings.Contains(got, "(0s)") || strings.Contains(got, "(1s)") || strings.Contains(got, "(2s)") {
		t.Fatalf("expected no elapsed time below threshold, got %q", got)
	}
}

func TestTransientStatusResetElapsed(t *testing.T) {
	var out bytes.Buffer
	status := &transientStatus{
		w:       &out,
		message: "step one",
		enabled: true,
		stopCh:  make(chan struct{}),
		doneCh:  make(chan struct{}),
		started: time.Now().Add(-10 * time.Second),
	}
	status.resetElapsed()

	go status.run(5 * time.Millisecond)
	time.Sleep(12 * time.Millisecond)
	status.stop()

	got := out.String()
	// After reset, elapsed should be near 0, below threshold — no "(10s)" in output
	if strings.Contains(got, "(10s)") || strings.Contains(got, "(9s)") {
		t.Fatalf("expected elapsed to be reset, got %q", got)
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

func TestAnnounceConfigPathSkipsWhenQuietOrEmpty(t *testing.T) {
	t.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", "1")

	var out bytes.Buffer
	done := announceConfigPathWithWriter(&out, "/tmp/.okdev.yaml", false)
	done(true)
	if out.Len() != 0 {
		t.Fatalf("expected quiet announce to suppress output, got %q", out.String())
	}

	done = announceConfigPathWithWriter(&out, "", false)
	done(true)
	if out.Len() != 0 {
		t.Fatalf("expected empty path to suppress output, got %q", out.String())
	}
}

func TestWithQuietConfigAnnounceSetsAndRestoresEnv(t *testing.T) {
	t.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", "0")
	if err := withQuietConfigAnnounce(func() error {
		if got := os.Getenv("OKDEV_QUIET_CONFIG_ANNOUNCE"); got != "1" {
			t.Fatalf("expected quiet env to be forced inside callback, got %q", got)
		}
		return nil
	}); err != nil {
		t.Fatalf("withQuietConfigAnnounce: %v", err)
	}
	if got := os.Getenv("OKDEV_QUIET_CONFIG_ANNOUNCE"); got != "0" {
		t.Fatalf("expected quiet env to be restored, got %q", got)
	}
}

func TestLoadConfigAndNamespaceAppliesOverrides(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(""+
		"apiVersion: okdev.io/v1alpha1\n"+
		"kind: DevEnvironment\n"+
		"metadata:\n"+
		"  name: demo\n"+
		"spec:\n"+
		"  namespace: team-a\n"+
		"  kubeContext: cluster-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	opts := &Options{ConfigPath: cfgPath, Namespace: "team-b"}
	cfg, ns, err := loadConfigAndNamespace(opts)
	if err != nil {
		t.Fatalf("loadConfigAndNamespace: %v", err)
	}
	if cfg.Metadata.Name != "demo" {
		t.Fatalf("unexpected config name %q", cfg.Metadata.Name)
	}
	if ns != "team-b" {
		t.Fatalf("expected namespace override, got %q", ns)
	}
	if opts.Context != "cluster-a" {
		t.Fatalf("expected kube context to be applied, got %q", opts.Context)
	}
}

func TestResolveCommandContextUsesResolver(t *testing.T) {
	tmp := t.TempDir()
	cfgPath := filepath.Join(tmp, ".okdev.yaml")
	if err := os.WriteFile(cfgPath, []byte(""+
		"apiVersion: okdev.io/v1alpha1\n"+
		"kind: DevEnvironment\n"+
		"metadata:\n"+
		"  name: demo\n"+
		"spec:\n"+
		"  namespace: team-a\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	cc, err := resolveCommandContext(&Options{ConfigPath: cfgPath}, func(_ *Options, cfg *config.DevEnvironment, ns string) (string, error) {
		if cfg.Metadata.Name != "demo" {
			t.Fatalf("unexpected config name %q", cfg.Metadata.Name)
		}
		if ns != "team-a" {
			t.Fatalf("unexpected namespace %q", ns)
		}
		return "sess-a", nil
	})
	if err != nil {
		t.Fatalf("resolveCommandContext: %v", err)
	}
	if cc.sessionName != "sess-a" {
		t.Fatalf("unexpected session %q", cc.sessionName)
	}
	if cc.cfgPath != cfgPath {
		t.Fatalf("unexpected cfg path %q", cc.cfgPath)
	}
	if cc.kube == nil {
		t.Fatal("expected kube client")
	}
}

func TestNewTransientStatusAndStartTransientStatusDisableForNonTTY(t *testing.T) {
	var out bytes.Buffer
	status := newTransientStatus(&out, "loading")
	if status.enabled {
		t.Fatal("expected non-terminal transient status to be disabled")
	}

	stop := startTransientStatus(&out, "loading")
	stop()
	if out.Len() != 0 {
		t.Fatalf("expected disabled transient status to remain silent, got %q", out.String())
	}
}

func TestDefaultContextAndInteractiveContext(t *testing.T) {
	ctx, cancel := defaultContext()
	defer cancel()
	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("expected default context to have deadline")
	}

	ictx, icancel := interactiveContext()
	defer icancel()
	if _, ok := ictx.Deadline(); ok {
		t.Fatal("expected interactive context to have no deadline")
	}
}

func TestCurrentOwnerAndSelector(t *testing.T) {
	t.Setenv("OKDEV_OWNER", "Team Ops")
	t.Setenv("USER", "ignored-user")

	if got := currentOwner(nil); got != "team-ops" {
		t.Fatalf("unexpected current owner %q", got)
	}
	if got := currentOwner(&Options{Owner: "Alice.Admin"}); got != "alice.admin" {
		t.Fatalf("unexpected owner override %q", got)
	}
	if got := normalizeOwner("  Bob / QA  "); got != "bob---qa" {
		t.Fatalf("unexpected normalized owner %q", got)
	}
	if got := ownerLabelSelector(&Options{Owner: "Alice Admin"}); got != "okdev.io/owner=alice-admin" {
		t.Fatalf("unexpected owner selector %q", got)
	}
}

func TestApplySessionArg(t *testing.T) {
	opts := &Options{}
	applySessionArg(opts, []string{"sess-a"})
	if opts.Session != "sess-a" {
		t.Fatalf("expected session to be applied, got %q", opts.Session)
	}

	opts = &Options{Session: "explicit"}
	applySessionArg(opts, []string{"ignored"})
	if opts.Session != "explicit" {
		t.Fatalf("expected explicit session to win, got %q", opts.Session)
	}
}

func TestOutputJSONPrettyPrints(t *testing.T) {
	var out bytes.Buffer
	if err := outputJSON(&out, map[string]any{"name": "demo", "count": 2}); err != nil {
		t.Fatalf("outputJSON: %v", err)
	}
	if !strings.Contains(out.String(), "\n  \"count\": 2,\n") {
		t.Fatalf("expected indented json, got %q", out.String())
	}
	var decoded map[string]any
	if err := json.Unmarshal(out.Bytes(), &decoded); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
}

func TestResolveSessionNameWithReaderReturnsActiveSessionWhenPodLookupFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveActiveSession("active-session"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.DefaultNameTemplate = "fresh-session"

	got, err := resolveSessionNameWithReader(&Options{}, cfg, "default", false, fakeSessionAccessReader{
		listErr: context.DeadlineExceeded,
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "active-session" {
		t.Fatalf("expected active session to be kept on lookup error, got %q", got)
	}
}
