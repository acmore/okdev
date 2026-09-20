package cli

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func TestDeathReportDoesNotCrossNamespaces(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSession(t, "sess1")
	if err := session.SaveLastSeen("sess1", session.LastSeen{At: time.Now(), Namespace: "default"}); err != nil {
		t.Fatal(err)
	}
	if report, ok := buildSessionDeathReport(context.Background(), &fakeEventLister{}, "sess1", "other"); ok {
		t.Fatalf("snapshot from default became a disappearance in other: %+v", report)
	}
}

func TestDeathReportContextAndRunIdentity(t *testing.T) {
	for _, tc := range []struct {
		name, snapshotContext, snapshotRun, context string
		want                                        bool
	}{
		{"matching", "dev", "run-a", "dev", true},
		{"another context", "dev", "run-a", "other", false},
		{"another run", "dev", "old-run", "dev", false},
		{"legacy matching context", "", "run-a", "dev", true},
		{"legacy wrong context", "", "run-a", "other", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			if err := session.SaveInfo(session.Info{Name: "sess", Namespace: "demo", KubeContext: "dev", RunID: "run-a"}); err != nil {
				t.Fatal(err)
			}
			if err := session.SaveLastSeen("sess", session.LastSeen{At: time.Now(), Namespace: "demo", Context: tc.snapshotContext, RunID: tc.snapshotRun}); err != nil {
				t.Fatal(err)
			}
			report, ok := buildSessionDeathReport(context.Background(), &fakeEventLister{}, "sess", "demo", tc.context)
			if ok != tc.want || (ok && (!report.Historical || report.Context != "dev" || report.Namespace != "demo")) {
				t.Fatalf("report=%+v ok=%v, want %v", report, ok, tc.want)
			}
		})
	}
}

func TestBareStatusUsesActiveConfigWithoutLosingExplicitOverrides(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OKDEV_CONFIG", "")
	repo := t.TempDir()
	t.Chdir(repo)
	saved := filepath.Join(repo, "saved", "okdev.yaml")
	discovered := filepath.Join(repo, ".okdev", "okdev.yaml")
	writeSelectionConfig(t, saved)
	writeSelectionConfig(t, discovered)
	if err := session.SaveInfo(session.Info{Name: "active", ConfigPath: saved, Namespace: "saved-ns", KubeContext: "saved-context"}); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveActiveSession("active"); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name            string
		opts            Options
		all             bool
		path, namespace string
	}{
		{"bare", Options{}, false, saved, "saved-ns"},
		{"namespace override", Options{Namespace: "override"}, false, saved, "override"},
		{"explicit config", Options{ConfigPath: discovered}, false, discovered, "default"},
		{"all sessions", Options{}, true, discovered, "default"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, err := resolveStatusContext(&tc.opts, tc.all)
			if err != nil || cc.cfgPath != tc.path || cc.namespace != tc.namespace || cc.opts.Session != "" {
				t.Fatalf("context=%+v err=%v", cc, err)
			}
		})
	}
}

func TestStatusDoesNotOverwriteHistoryFromAnotherNamespace(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSession(t, "sess1")
	before := session.LastSeen{At: time.Now().UTC(), Namespace: "default", Pods: []session.LastSeenPod{{Name: "original"}}}
	if err := session.SaveLastSeen("sess1", before); err != nil {
		t.Fatal(err)
	}
	captureSessionLastSeen([]sessionView{{Session: "sess1", Namespace: "other", Pods: []kube.PodSummary{{Name: "unrelated"}}}})
	after, err := session.LoadLastSeen("sess1")
	if err != nil || !after.At.Equal(before.At) || after.Pods[0].Name != "original" {
		t.Fatalf("unrelated live session overwrote history: %+v, %v", after, err)
	}
}
