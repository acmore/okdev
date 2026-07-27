package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
)

func rotateTestState(t *testing.T, sessionName string) *upState {
	t.Helper()
	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Type = workload.TypePod
	return &upState{
		runtime: &fakeRefRuntime{kind: workload.TypePod, apiVersion: "v1", name: "okdev-" + sessionName + "-oldrun1"},
		labels: map[string]string{
			"okdev.io/session": sessionName,
			"okdev.io/run-id":  "oldrun1",
		},
		annotations:  map[string]string{},
		runID:        "oldrun1",
		workloadName: "okdev-" + sessionName + "-oldrun1",
		command: &commandContext{
			namespace:   "default",
			sessionName: sessionName,
			cfg:         cfg,
		},
	}
}

func TestRotateRunIdentityMintsAFreshRun(t *testing.T) {
	state := rotateTestState(t, "sess")
	if err := rotateRunIdentity(state); err != nil {
		t.Fatalf("rotateRunIdentity: %v", err)
	}

	if state.runID == "oldrun1" || strings.TrimSpace(state.runID) == "" {
		t.Fatalf("expected a fresh run id, got %q", state.runID)
	}
	if state.workloadName == "okdev-sess-oldrun1" {
		t.Fatalf("expected a fresh workload name, got %q", state.workloadName)
	}
	if !strings.HasPrefix(state.workloadName, "okdev-sess-") {
		t.Fatalf("workload name must stay session-scoped, got %q", state.workloadName)
	}
	// The discovery label has to follow the object, or `okdev status` and the
	// pod waits would keep selecting on the dead run.
	if got := state.labels["okdev.io/run-id"]; got != state.runID {
		t.Fatalf("run-id label = %q, want %q", got, state.runID)
	}
	if state.labels["okdev.io/session"] != "sess" {
		t.Fatal("session label must survive the rotation")
	}
	// The runtime must be rebuilt: the apply after this call has to target the
	// new name, while the delete before it targeted the old one.
	if state.runtime.WorkloadName() != state.workloadName {
		t.Fatalf("runtime name = %q, want %q", state.runtime.WorkloadName(), state.workloadName)
	}
	// A recreate okdev performed is not a run that died on its own (#213).
	if !state.recreatedThisRun {
		t.Fatal("rotation must mark the recreate as okdev-initiated")
	}
}

func TestRotateRunIdentityIsUniquePerCall(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 8; i++ {
		state := rotateTestState(t, "sess")
		if err := rotateRunIdentity(state); err != nil {
			t.Fatalf("rotateRunIdentity: %v", err)
		}
		if seen[state.runID] {
			t.Fatalf("run id %q repeated across recreates", state.runID)
		}
		seen[state.runID] = true
	}
}
