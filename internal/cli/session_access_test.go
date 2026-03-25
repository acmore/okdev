package cli

import (
	"context"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type fakeSessionAccessReader struct {
	pod *kube.PodSummary
	err error
}

func (f fakeSessionAccessReader) GetPodSummary(_ context.Context, _, _ string) (*kube.PodSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	return f.pod, nil
}

func (f fakeSessionAccessReader) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.pod == nil {
		return nil, nil
	}
	return []kube.PodSummary{*f.pod}, nil
}

func TestEnsureSessionAccessRequiresExistingSessionWhenRequested(t *testing.T) {
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{}, "default", "old", true, true)
	if err == nil || !strings.Contains(err.Error(), `session "old" does not exist in namespace "default"`) {
		t.Fatalf("expected missing session error, got %v", err)
	}
}

func TestEnsureSessionAccessAllowsMissingSessionWhenNotRequired(t *testing.T) {
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{}, "default", "old", true, false)
	if err != nil {
		t.Fatalf("expected missing session to be allowed, got %v", err)
	}
}

func TestEnsureSessionAccessRejectsOtherOwner(t *testing.T) {
	t.Setenv("USER", "alice")
	err := ensureSessionAccess(&Options{}, fakeSessionAccessReader{
		pod: &kube.PodSummary{
			Labels: map[string]string{
				"okdev.io/owner":     "bob",
				"okdev.io/shareable": "false",
			},
			Annotations: map[string]string{},
		},
	}, "default", "team", false, true)
	if err == nil || !strings.Contains(err.Error(), `session "team" is owned by "bob"`) {
		t.Fatalf("expected owner mismatch error, got %v", err)
	}
}

func TestEnsureSessionAccessAllowsShareableOtherOwner(t *testing.T) {
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
	}, "default", "team", true, true)
	if err != nil {
		t.Fatalf("expected shareable session to be allowed, got %v", err)
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
	}, "default", "team", false, true)
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
		err: apierrors.NewNotFound(schema.GroupResource{Group: "", Resource: "pods"}, "okdev-old-session"),
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
