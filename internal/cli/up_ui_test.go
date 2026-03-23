package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestUpUIInteractiveStepRunUpdatesInPlace(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{
		out:         &out,
		errOut:      &out,
		interactive: true,
	}

	ui.stepRun("pod readiness", "Pending 0/2 (ContainerCreating)")
	ui.stepRun("pod readiness", "Running 1/2 (-)")
	ui.stepDone("pod readiness", "ready")

	got := out.String()
	if !strings.Contains(got, "pod readiness: Running 1/2 (-)") {
		t.Fatalf("expected updated transient step message, got %q", got)
	}
	if !strings.Contains(got, "✔ pod readiness: ready\n") {
		t.Fatalf("expected final done line, got %q", got)
	}
	if !strings.Contains(got, "\r\033[K") {
		t.Fatalf("expected transient clear sequence, got %q", got)
	}
}
