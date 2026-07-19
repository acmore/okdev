package cli

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/session"
)

func TestTransientClusterFailureEscalatesAfterSustainedStreak(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	cause := errors.New("dial tcp: i/o timeout")

	// Failures 1 and 2: plain transient framing, still "retry" guidance.
	err1 := transientClusterFailure("sess1", cause, base)
	if !errors.Is(err1, ErrTransientCluster) || strings.Contains(err1.Error(), "okdev-transient-streak") {
		t.Fatalf("first failure must not escalate: %v", err1)
	}
	err2 := transientClusterFailure("sess1", cause, base.Add(30*time.Second))
	if strings.Contains(err2.Error(), "okdev-transient-streak") {
		t.Fatalf("second failure under the span must not escalate: %v", err2)
	}

	// Third consecutive failure, 90s after the first: escalate.
	err3 := transientClusterFailure("sess1", cause, base.Add(90*time.Second))
	if !errors.Is(err3, ErrTransientCluster) {
		t.Fatalf("escalated error must keep the transient sentinel (exit 78 contract): %v", err3)
	}
	msg := err3.Error()
	if !strings.Contains(msg, "okdev-transient-streak: count=3 since=2026-07-19T10:00:00Z") {
		t.Fatalf("expected machine-readable streak marker, got: %v", msg)
	}
	if !strings.Contains(msg, "sustained network/cluster outage") {
		t.Fatalf("expected escalation guidance, got: %v", msg)
	}
	if code, ok := ClassifiedExitCode(err3); !ok || code != ExitTransientCluster {
		t.Fatalf("escalated failure must stay exit %d, got %d/%v", ExitTransientCluster, code, ok)
	}
}

func TestTransientClusterFailureFastRetriesDoNotEscalate(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	cause := errors.New("connection refused")
	// Five failures within two seconds: one blip retried hard, not an outage.
	var last error
	for i := 0; i < 5; i++ {
		last = transientClusterFailure("sess1", cause, base.Add(time.Duration(i)*400*time.Millisecond))
	}
	if strings.Contains(last.Error(), "okdev-transient-streak") {
		t.Fatalf("rapid retries under the span threshold must not escalate: %v", last)
	}
}

func TestTransientStreakClearsOnSuccess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	base := time.Date(2026, 7, 19, 10, 0, 0, 0, time.UTC)
	for i := 0; i < 3; i++ {
		_, _ = session.RecordTransientFailure("sess1", base.Add(time.Duration(i)*time.Minute))
	}
	if err := session.ClearTransientStreak("sess1"); err != nil {
		t.Fatal(err)
	}
	// The next failure starts a fresh streak: no escalation.
	err := transientClusterFailure("sess1", errors.New("timeout"), base.Add(10*time.Minute))
	if strings.Contains(err.Error(), "okdev-transient-streak") {
		t.Fatalf("cleared streak must reset escalation, got: %v", err)
	}
}
