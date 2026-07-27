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

func seedRunSession(t *testing.T, name, runID string) {
	t.Helper()
	if err := session.SaveInfo(session.Info{
		Name:               name,
		Namespace:          "default",
		RunID:              runID,
		WorkloadAPIVersion: "kubeflow.org/v1",
		WorkloadKind:       "PyTorchJob",
		WorkloadName:       "okdev-" + name + "-" + runID,
	}); err != nil {
		t.Fatal(err)
	}
}

func TestClassifyRunEndEvent(t *testing.T) {
	tests := []struct {
		name    string
		reason  string
		message string
		want    string
	}{
		// The distinction that matters: an idle reclaim and a preemption call
		// for opposite mitigations (#213).
		{name: "custom idle reclaim controller", reason: "WorkloadReclaimed", message: "reclaiming idle workload after 4h", want: session.RunEndClassIdleReclaim},
		{name: "ttl expiry", reason: "TTLExpired", message: "workload exceeded its time limit", want: session.RunEndClassIdleReclaim},
		{name: "preemption", reason: "Preempted", message: "Preempted by higher priority pod", want: session.RunEndClassEvicted},
		{name: "eviction", reason: "Evicted", message: "The node was low on resource: memory", want: session.RunEndClassEvicted},
		{name: "quota", reason: "FailedCreate", message: "exceeded quota: gpu", want: session.RunEndClassEvicted},
		{name: "oom", reason: "OOMKilling", message: "Container dev was OOM killed", want: session.RunEndClassOOM},
		{name: "external delete", reason: "Killing", message: "Stopping container dev", want: session.RunEndClassDeleted},
		// A start-side event must not be dressed up as a cause of death: this
		// message says "Insufficient", which the keyword pass would otherwise
		// read as preemption.
		{name: "failed scheduling is not an end", reason: "FailedScheduling", message: "0/3 nodes available: Insufficient nvidia.com/gpu", want: session.RunEndClassUnknown},
		{name: "image pull backoff is not an end", reason: "BackOff", message: "Back-off pulling image", want: session.RunEndClassUnknown},
		{name: "plain lifecycle noise", reason: "Started", message: "Started container dev", want: session.RunEndClassUnknown},
		{name: "empty", reason: "", message: "", want: session.RunEndClassUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := classifyRunEndEvent(tt.reason, tt.message); got != tt.want {
				t.Fatalf("classify(%q, %q) = %q, want %q", tt.reason, tt.message, got, tt.want)
			}
		})
	}
}

func TestDetectPreviousRunEndReportsOnlyForeignRuns(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRunSession(t, "sess1", "newrun22")
	if err := session.SaveLastSeen("sess1", session.LastSeen{
		At:        time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC),
		RunID:     "oldrun11",
		Namespace: "default",
		Workload:  session.LastSeenWorkload{Kind: "PyTorchJob", Name: "okdev-sess1-oldrun11"},
		Pods:      []session.LastSeenPod{{Name: "okdev-sess1-oldrun11-master-0", Phase: "Running", Reason: "-"}},
	}); err != nil {
		t.Fatal(err)
	}
	lister := &fakeEventLister{events: map[string][]kube.EventSummary{
		"okdev-sess1-oldrun11": {
			{Type: "Normal", Reason: "WorkloadReclaimed", Message: "reclaiming idle workload", InvolvedName: "okdev-sess1-oldrun11"},
		},
	}}

	record, ok := detectPreviousRunEnd(context.Background(), lister, "sess1", "default", "newrun22", "okdev-sess1-newrun22")
	if !ok {
		t.Fatal("expected a report when the snapshot describes a different run")
	}
	if record.EndedRunID != "oldrun11" || record.SucceededByRunID != "newrun22" {
		t.Fatalf("unexpected identity in %+v", record)
	}
	if record.Class != session.RunEndClassIdleReclaim {
		t.Fatalf("class = %q, want idle reclaim", record.Class)
	}
	if !strings.Contains(record.Evidence, "WorkloadReclaimed") {
		t.Fatalf("evidence should quote the event, got %q", record.Evidence)
	}

	// The same run coming back up (reuse, --reconcile, drift re-apply all keep
	// the run-id) is not an ended run and must stay silent.
	if _, ok := detectPreviousRunEnd(context.Background(), lister, "sess1", "default", "oldrun11", "okdev-sess1-oldrun11"); ok {
		t.Fatal("a snapshot for the current run must not report an end")
	}
}

func TestDetectPreviousRunEndWithoutSnapshotOrEvents(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRunSession(t, "sess1", "newrun22")

	// No snapshot at all: an intentional teardown (down, restart) clears it,
	// so there is nothing to explain.
	if _, ok := detectPreviousRunEnd(context.Background(), &fakeEventLister{}, "sess1", "default", "newrun22", "okdev-sess1-newrun22"); ok {
		t.Fatal("expected no report without a snapshot")
	}

	// Snapshot but no surviving events and no cached termination detail: still
	// report, because "the run you are on is not the run you had" is itself
	// the finding — it just cannot name a cause.
	if err := session.SaveLastSeen("sess1", session.LastSeen{
		At:        time.Now().UTC().Add(-2 * time.Hour),
		RunID:     "oldrun11",
		Namespace: "default",
		Workload:  session.LastSeenWorkload{Kind: "PyTorchJob", Name: "okdev-sess1-oldrun11"},
		Pods:      []session.LastSeenPod{{Name: "okdev-sess1-oldrun11-master-0", Phase: "Running", Reason: "-"}},
	}); err != nil {
		t.Fatal(err)
	}
	record, ok := detectPreviousRunEnd(context.Background(), &fakeEventLister{}, "sess1", "default", "newrun22", "okdev-sess1-newrun22")
	if !ok {
		t.Fatal("expected a report even when the cause is unknown")
	}
	if record.Class != session.RunEndClassUnknown {
		t.Fatalf("class = %q, want unknown", record.Class)
	}
	var buf bytes.Buffer
	printPreviousRunEnd(&buf, record)
	got := buf.String()
	if !strings.Contains(got, "previous run oldrun11 ended:") || !strings.Contains(got, "cause unknown") {
		t.Fatalf("unknown-cause line = %q", got)
	}
	if !strings.Contains(got, "expire ~1h") {
		t.Fatalf("unknown-cause line should explain why nothing is known, got %q", got)
	}
}

func TestDetectPreviousRunEndFallsBackToWorkloadName(t *testing.T) {
	// Snapshots written before run-ids were recorded still prove a different
	// run through the workload name, which carries the run suffix.
	t.Setenv("HOME", t.TempDir())
	seedRunSession(t, "sess1", "newrun22")
	if err := session.SaveLastSeen("sess1", session.LastSeen{
		At:        time.Now().UTC().Add(-time.Hour),
		Namespace: "default",
		Workload:  session.LastSeenWorkload{Kind: "PyTorchJob", Name: "okdev-sess1-oldrun11"},
		Pods:      []session.LastSeenPod{{Name: "okdev-sess1-oldrun11-master-0", Phase: "Failed", Reason: "Evicted"}},
	}); err != nil {
		t.Fatal(err)
	}
	record, ok := detectPreviousRunEnd(context.Background(), &fakeEventLister{}, "sess1", "default", "newrun22", "okdev-sess1-newrun22")
	if !ok {
		t.Fatal("expected a report from a pre-run-id snapshot")
	}
	if record.Class != session.RunEndClassEvicted {
		t.Fatalf("class = %q, want evicted from the cached pod reason", record.Class)
	}
	var buf bytes.Buffer
	printPreviousRunEnd(&buf, record)
	if got := buf.String(); !strings.Contains(got, "okdev-sess1-oldrun11") || !strings.Contains(got, "chunk long jobs") {
		t.Fatalf("expected the workload name and the eviction mitigation, got %q", got)
	}
}

func TestPreviousRunEndScopedToTheRunItExplains(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	seedRunSession(t, "sess1", "run2")
	if err := session.SaveRunEnd("sess1", session.RunEnd{
		EndedRunID:       "run1",
		SucceededByRunID: "run2",
		Class:            session.RunEndClassEvicted,
		Reason:           "evicted or preempted",
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := currentSessionPreviousRunEnd("sess1"); !ok {
		t.Fatal("expected the record while run2 is live")
	}

	// A third run supersedes it: the report belongs to the recreate it
	// explains, not to the session forever.
	seedRunSession(t, "sess1", "run3")
	if _, ok := currentSessionPreviousRunEnd("sess1"); ok {
		t.Fatal("a stale record must not follow the session into a later run")
	}
}
