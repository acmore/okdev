package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// flakySessionAccessReader returns the queued errs in order (one per ListPods
// call); once exhausted it returns pods. It records the call count so tests
// can assert retry behavior.
type flakySessionAccessReader struct {
	errs  []error
	pods  []kube.PodSummary
	calls int
}

func (f *flakySessionAccessReader) GetPodSummary(_ context.Context, _, _ string) (*kube.PodSummary, error) {
	return nil, nil
}

func (f *flakySessionAccessReader) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	idx := f.calls
	f.calls++
	if idx < len(f.errs) {
		return nil, f.errs[idx]
	}
	return f.pods, nil
}

func (f *flakySessionAccessReader) ListResources(_ context.Context, _ string, _ bool, _, _, _ string) ([]kube.ResourceSummary, error) {
	return nil, nil
}

func (f *flakySessionAccessReader) ResourceExists(_ context.Context, _, _, _, _ string) (bool, error) {
	return false, nil
}

type fakeSessionAccessReader struct {
	pod            *kube.PodSummary
	pods           []kube.PodSummary
	resources      []kube.ResourceSummary
	getErr         error
	listErr        error
	resourceExists bool
	resourceErr    error
}

func (f fakeSessionAccessReader) GetPodSummary(_ context.Context, _, _ string) (*kube.PodSummary, error) {
	if f.getErr != nil {
		return nil, f.getErr
	}
	return f.pod, nil
}

func (f fakeSessionAccessReader) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	if f.pods != nil {
		return f.pods, nil
	}
	if f.pod == nil {
		return nil, nil
	}
	return []kube.PodSummary{*f.pod}, nil
}

func (f fakeSessionAccessReader) ListResources(_ context.Context, _ string, _ bool, _, _, _ string) ([]kube.ResourceSummary, error) {
	if f.listErr != nil {
		return nil, f.listErr
	}
	return f.resources, nil
}

func (f fakeSessionAccessReader) ResourceExists(_ context.Context, _, _, _, _ string) (bool, error) {
	if f.resourceErr != nil {
		return false, f.resourceErr
	}
	return f.resourceExists, nil
}

func TestEnsureSessionAccessRequiresExistingSessionWhenRequested(t *testing.T) {
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{}, "default", "old", true)
	if err == nil || !strings.Contains(err.Error(), `session "old" does not exist in namespace "default"`) {
		t.Fatalf("expected missing session error, got %v", err)
	}
	if !errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("expected ErrSessionNotFound sentinel, got %v", err)
	}
	if code, ok := ClassifiedExitCode(err); !ok || code != ExitSessionNotFound {
		t.Fatalf("expected exit %d for not-found, got code=%d ok=%v", ExitSessionNotFound, code, ok)
	}
}

func TestEnsureSessionAccessRetriesTransientThenSucceeds(t *testing.T) {
	reader := &flakySessionAccessReader{
		errs: []error{
			apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "list", 1),
		},
		pods: []kube.PodSummary{{
			Labels:      map[string]string{"okdev.io/owner": ""},
			Annotations: map[string]string{},
		}},
	}
	if err := ensureSessionAccess(&Options{}, reader, "default", "team", true); err != nil {
		t.Fatalf("expected transient error to be retried into success, got %v", err)
	}
	if reader.calls != 2 {
		t.Fatalf("expected one retry (2 calls), got %d", reader.calls)
	}
}

func TestEnsureSessionAccessClassifiesPersistentTransientFailure(t *testing.T) {
	reader := &flakySessionAccessReader{
		errs: []error{
			apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "list", 1),
			apierrors.NewServerTimeout(schema.GroupResource{Resource: "pods"}, "list", 1),
		},
	}
	err := ensureSessionAccess(&Options{}, reader, "default", "team", true)
	if !errors.Is(err, ErrTransientCluster) {
		t.Fatalf("expected ErrTransientCluster sentinel, got %v", err)
	}
	if code, ok := ClassifiedExitCode(err); !ok || code != ExitTransientCluster {
		t.Fatalf("expected exit %d for transient, got code=%d ok=%v", ExitTransientCluster, code, ok)
	}
	if reader.calls != 2 {
		t.Fatalf("expected exactly one retry (2 calls), got %d", reader.calls)
	}
}

func TestEnsureSessionAccessDoesNotRetryPermanentFailure(t *testing.T) {
	reader := &flakySessionAccessReader{
		errs: []error{
			apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "list", errors.New("rbac")),
		},
	}
	err := ensureSessionAccess(&Options{}, reader, "default", "team", true)
	if errors.Is(err, ErrTransientCluster) || errors.Is(err, ErrSessionNotFound) {
		t.Fatalf("permanent failure must not be classified as transient/not-found, got %v", err)
	}
	if _, ok := ClassifiedExitCode(err); ok {
		t.Fatalf("permanent failure should fall through to exit 1, got classified code")
	}
	if reader.calls != 1 {
		t.Fatalf("expected no retry on permanent failure (1 call), got %d", reader.calls)
	}
}

func TestEnsureSessionAccessAllowsMissingSessionWhenNotRequired(t *testing.T) {
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{}, "default", "old", false)
	if err != nil {
		t.Fatalf("expected missing session to be allowed, got %v", err)
	}
}

func TestEnsureSessionAccessRejectsOtherOwner(t *testing.T) {
	t.Setenv("USER", "alice")
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{
		pod: &kube.PodSummary{
			Labels: map[string]string{
				"okdev.io/owner": "bob",
			},
			Annotations: map[string]string{},
		},
	}, "default", "team", true)
	if err == nil || !strings.Contains(err.Error(), `session "team" is owned by "bob"`) {
		t.Fatalf("expected owner mismatch error, got %v", err)
	}
}

func TestEnsureSessionAccessRejectsOtherOwnerEvenIfShareableMetadataExists(t *testing.T) {
	t.Setenv("USER", "alice")
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{
		pod: &kube.PodSummary{
			Labels: map[string]string{
				"okdev.io/owner":     "bob",
				"okdev.io/shareable": "true",
			},
			Annotations: map[string]string{
				"okdev.io/shareable": "true",
			},
		},
	}, "default", "team", true)
	if err == nil || !strings.Contains(err.Error(), `session "team" is owned by "bob"`) {
		t.Fatalf("expected owner mismatch error, got %v", err)
	}
}

func TestEnsureSessionAccessAllowsCurrentOwner(t *testing.T) {
	t.Setenv("USER", "alice")
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{
		pod: &kube.PodSummary{
			Labels: map[string]string{
				"okdev.io/owner": "alice",
			},
			Annotations: map[string]string{},
		},
	}, "default", "team", true)
	if err != nil {
		t.Fatalf("expected current owner to be allowed, got %v", err)
	}
}

func TestResolveSessionNameWithReaderIgnoresStaleActiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveActiveSession("old-session"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.DefaultNameTemplate = "fresh-session"

	got, err := resolveSessionNameWithReader(&Options{}, cfg, "default", false, fakeSessionAccessReader{
		getErr: apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "okdev-old-session"),
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "fresh-session" {
		t.Fatalf("expected stale active session to be ignored, got %q", got)
	}

	active, err := session.LoadActiveSession()
	if err != nil {
		t.Fatalf("LoadActiveSession: %v", err)
	}
	if active != "" {
		t.Fatalf("expected stale active session to be cleared, got %q", active)
	}
}

func TestResolveSessionNameWithReaderKeepsControllerBackedActiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveActiveSession("job-session"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.DefaultNameTemplate = "fresh-session"

	got, err := resolveSessionNameWithReader(&Options{}, cfg, "default", false, fakeSessionAccessReader{
		getErr: apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "okdev-job-session"),
		pods: []kube.PodSummary{{
			Name: "job-session-abc123",
			Labels: map[string]string{
				"okdev.io/managed": "true",
				"okdev.io/session": "job-session",
			},
		}},
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "job-session" {
		t.Fatalf("expected controller-backed active session to be preserved, got %q", got)
	}
}

func TestResolveSessionNameWithReaderIgnoresActiveSessionFromDifferentConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot, err := session.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}
	if err := session.SaveActiveSession("sess-a"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}
	if err := session.SaveInfo(session.Info{
		Name:       "sess-a",
		RepoRoot:   repoRoot,
		ConfigPath: filepath.Join(repoRoot, "service-a", ".okdev.yaml"),
		Namespace:  "default",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Spec.Session.DefaultNameTemplate = "fresh-session"

	got, err := resolveSessionNameWithReader(&Options{
		ConfigPath: filepath.Join(repoRoot, "service-b", ".okdev.yaml"),
	}, cfg, "default", false, fakeSessionAccessReader{
		pods: []kube.PodSummary{{
			Name: "okdev-sess-a",
			Labels: map[string]string{
				"okdev.io/managed": "true",
				"okdev.io/session": "sess-a",
			},
		}},
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "fresh-session" {
		t.Fatalf("expected different-config active session to be ignored, got %q", got)
	}
}

func TestResolveSessionNameWithReaderInfersSessionMatchingCurrentConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot, err := session.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	currentConfig := filepath.Join(repoRoot, "Mooncake", ".okdev.yaml")
	otherConfig := filepath.Join(repoRoot, "Elsewhere", ".okdev.yaml")
	if err := session.SaveInfo(session.Info{
		Name:       "older-match",
		RepoRoot:   repoRoot,
		ConfigPath: currentConfig,
		Namespace:  "default",
	}); err != nil {
		t.Fatalf("SaveInfo current match: %v", err)
	}
	if err := session.SaveInfo(session.Info{
		Name:       "newer-mismatch",
		RepoRoot:   repoRoot,
		ConfigPath: otherConfig,
		Namespace:  "default",
	}); err != nil {
		t.Fatalf("SaveInfo other match: %v", err)
	}

	cfg := &config.DevEnvironment{}
	got, err := resolveSessionNameWithReader(&Options{
		ConfigPath: currentConfig,
	}, cfg, "default", true, fakeSessionAccessReader{
		pods: []kube.PodSummary{
			{
				Name:      "okdev-newer-mismatch",
				CreatedAt: time.Now(),
				Labels: map[string]string{
					"okdev.io/managed": "true",
					"okdev.io/session": "newer-mismatch",
					"okdev.io/repo":    filepath.Base(repoRoot),
				},
			},
			{
				Name:      "okdev-older-match",
				CreatedAt: time.Now().Add(-time.Hour),
				Labels: map[string]string{
					"okdev.io/managed": "true",
					"okdev.io/session": "older-match",
					"okdev.io/repo":    filepath.Base(repoRoot),
				},
			},
		},
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "older-match" {
		t.Fatalf("expected config-matching inferred session, got %q", got)
	}
}

func TestResolveSessionNameWithReaderDefaultNameAnchorsOnConfigDir(t *testing.T) {
	// With no active session and nothing to infer, the default name must be
	// derived from the directory that owns the discovered config, not from the
	// git/CWD basename — so running from a repo subdirectory (where git may
	// report a different toplevel) resolves the same name as from the root.
	t.Setenv("HOME", t.TempDir())
	t.Setenv("USER", "alice")
	repoRoot, err := session.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	currentConfig := filepath.Join(repoRoot, "someproj", ".okdev", "okdev.yaml")
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "someproj"
	got, err := resolveSessionNameWithReader(&Options{
		ConfigPath: currentConfig,
		Owner:      "alice",
	}, cfg, "default", true, fakeSessionAccessReader{
		pods:           []kube.PodSummary{},
		resourceExists: false,
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "someproj-alice" {
		t.Fatalf("expected config-dir-anchored default name, got %q", got)
	}
}

func TestResolveSessionNameWithReaderInfersSavedControllerBackedSessionWhenNoPodsExistYet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot, err := session.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	currentConfig := filepath.Join(repoRoot, "Mooncake", ".okdev.yaml")
	if err := session.SaveInfo(session.Info{
		Name:               "pending-job",
		RepoRoot:           repoRoot,
		ConfigPath:         currentConfig,
		Namespace:          "default",
		Owner:              "alice",
		WorkloadType:       "job",
		WorkloadAPIVersion: "batch/v1",
		WorkloadKind:       "Job",
		WorkloadName:       "trainer",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "demo"
	got, err := resolveSessionNameWithReader(&Options{
		ConfigPath: currentConfig,
		Owner:      "alice",
	}, cfg, "default", true, fakeSessionAccessReader{
		pods:           []kube.PodSummary{},
		resourceExists: true,
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "pending-job" {
		t.Fatalf("expected saved controller-backed session, got %q", got)
	}
}

func TestResolveSessionNameWithReaderInfersLiveControllerBackedSessionWhenNoPodsExistYet(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repoRoot, err := session.RepoRoot()
	if err != nil {
		t.Fatalf("RepoRoot: %v", err)
	}

	currentConfig := filepath.Join(repoRoot, "Mooncake", ".okdev.yaml")
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "demo"
	got, err := resolveSessionNameWithReader(&Options{
		ConfigPath: currentConfig,
		Owner:      "alice",
	}, cfg, "default", true, fakeSessionAccessReader{
		pods: []kube.PodSummary{},
		resources: []kube.ResourceSummary{{
			Namespace:  "default",
			Name:       "trainer",
			Kind:       "Job",
			APIVersion: "batch/v1",
			CreatedAt:  time.Now(),
			Labels: map[string]string{
				"okdev.io/managed":       "true",
				"okdev.io/session":       "pending-job",
				"okdev.io/owner":         "alice",
				"okdev.io/repo":          filepath.Base(repoRoot),
				"okdev.io/name":          "demo",
				"okdev.io/workload-type": "job",
			},
		}},
	})
	if err != nil {
		t.Fatalf("resolveSessionNameWithReader: %v", err)
	}
	if got != "pending-job" {
		t.Fatalf("expected live controller-backed session, got %q", got)
	}
}
