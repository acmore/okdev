package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

type fakeEventLister struct {
	events map[string][]kube.EventSummary
}

func (f *fakeEventLister) ListObjectEvents(_ context.Context, _ string, name string) ([]kube.EventSummary, error) {
	return f.events[name], nil
}

func seedSession(t *testing.T, name string) {
	t.Helper()
	if err := session.SaveInfo(session.Info{
		Name:               name,
		Namespace:          "default",
		WorkloadAPIVersion: "kubeflow.org/v1",
		WorkloadKind:       "PyTorchJob",
		WorkloadName:       "okdev-" + name + "-abc",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestCaptureSessionLastSeenPersistsSnapshotForTrackedSessions(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSession(t, "sess1")

	captureSessionLastSeen([]sessionView{
		{
			Session:   "sess1",
			Namespace: "default",
			Pods: []kube.PodSummary{{
				Name:   "okdev-sess1-abc-worker-0",
				Phase:  "Running",
				Ready:  "2/2",
				Reason: "-",
				ContainerIssues: []kube.ContainerIssue{
					{Container: "pytorch", Reason: "OOMKilled", ExitCode: 137},
				},
			}},
		},
		{Session: "untracked", Pods: []kube.PodSummary{{Name: "x"}}},
	})

	snapshot, err := session.LoadLastSeen("sess1")
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.At.IsZero() || len(snapshot.Pods) != 1 {
		t.Fatalf("expected persisted snapshot with one pod, got %+v", snapshot)
	}
	if snapshot.Workload.Name != "okdev-sess1-abc" || snapshot.Pods[0].Issues[0].Reason != "OOMKilled" {
		t.Fatalf("snapshot missing workload/issue detail: %+v", snapshot)
	}

	// Untracked sessions (no session.json) must not grow state dirs.
	other, err := session.LoadLastSeen("untracked")
	if err != nil {
		t.Fatal(err)
	}
	if !other.At.IsZero() {
		t.Fatalf("expected no snapshot for untracked session, got %+v", other)
	}
}

func TestBuildSessionDeathReportUsesSnapshotAndEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSession(t, "sess1")
	if err := session.SaveLastSeen("sess1", session.LastSeen{
		At:        time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC),
		Namespace: "default",
		Workload:  session.LastSeenWorkload{Kind: "PyTorchJob", Name: "okdev-sess1-abc"},
		Pods: []session.LastSeenPod{
			{Name: "okdev-sess1-abc-worker-0", Phase: "Running", Reason: "-"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	lister := &fakeEventLister{events: map[string][]kube.EventSummary{
		"okdev-sess1-abc-worker-0": {
			{Type: "Warning", Reason: "Evicted", Message: "Pod was evicted due to memory pressure", InvolvedName: "okdev-sess1-abc-worker-0"},
		},
	}}

	report, ok := buildSessionDeathReport(context.Background(), lister, "sess1", "default")
	if !ok {
		t.Fatal("expected a death report when a snapshot exists")
	}
	if report.Found || report.Session != "sess1" || len(report.Pods) != 1 {
		t.Fatalf("unexpected report %+v", report)
	}
	if len(report.Events) != 1 || report.Events[0].Reason != "Evicted" {
		t.Fatalf("expected the eviction event, got %+v", report.Events)
	}

	var out bytes.Buffer
	printSessionDeathReport(&out, report)
	text := out.String()
	for _, want := range []string{"Last known state", "okdev-sess1-abc", "worker-0", "Evicted", "memory pressure"} {
		if !strings.Contains(text, want) {
			t.Fatalf("report text missing %q:\n%s", want, text)
		}
	}
}

func TestBuildSessionDeathReportWithoutSnapshotReportsNothing(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if _, ok := buildSessionDeathReport(context.Background(), &fakeEventLister{}, "ghost", "default"); ok {
		t.Fatal("expected no report without a cached snapshot")
	}
}

func TestLastSeenClearedOnDownAndUpPaths(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedSession(t, "sess1")
	if err := session.SaveLastSeen("sess1", session.LastSeen{At: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := session.ClearLastSeen("sess1"); err != nil {
		t.Fatal(err)
	}
	snapshot, err := session.LoadLastSeen("sess1")
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.At.IsZero() {
		t.Fatalf("expected cleared snapshot, got %+v", snapshot)
	}
}
