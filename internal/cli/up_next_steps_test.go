package cli

import (
	"bytes"
	"strings"
	"testing"
)

func subjects(hints []nextStepHint) []string {
	out := make([]string, 0, len(hints))
	for _, hint := range hints {
		out = append(out, hint.Subject)
	}
	return out
}

func TestBuildUpNextStepsIsShapeAware(t *testing.T) {
	tests := []struct {
		name          string
		hasSync       bool
		hasPostSync   bool
		hasPostCreate bool
		waitHooks     bool
		podCount      int
		want          []string
		absent        []string
	}{
		{
			// A single-pod session with no sync has no use for sync-convergence
			// or fanout hints; advertising them is what makes a block like this
			// get skipped wholesale.
			name:     "single pod, no sync, no hooks",
			podCount: 1,
			want:     []string{"spec.lifecycle.postCreate / postSync", "okdev jobs logs --tail --grep"},
			absent:   []string{"okdev sync wait", "okdev exec --all | --role worker", "master-0 / worker-1"},
		},
		{
			name:     "sync configured surfaces the convergence primitives",
			hasSync:  true,
			podCount: 1,
			want:     []string{"okdev sync wait / exec --require-sync"},
		},
		{
			name:          "hooks already configured on a multi-pod session point at convergence",
			hasSync:       true,
			hasPostSync:   true,
			hasPostCreate: true,
			podCount:      3,
			want:          []string{"okdev up --wait-hooks", "okdev exec --all | --role worker", "master-0 / worker-1"},
			absent:        []string{"spec.lifecycle.postCreate / postSync"},
		},
		{
			// --wait-hooks was just used, so repeating it back is noise.
			name:        "wait-hooks already used",
			hasSync:     true,
			hasPostSync: true,
			waitHooks:   true,
			podCount:    3,
			absent:      []string{"okdev up --wait-hooks"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := subjects(buildUpNextSteps(tt.hasSync, tt.hasPostSync, tt.hasPostCreate, tt.waitHooks, tt.podCount))
			joined := strings.Join(got, "\n")
			for _, want := range tt.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("expected %q in %v", want, got)
				}
			}
			for _, absent := range tt.absent {
				if strings.Contains(joined, absent) {
					t.Fatalf("did not expect %q in %v", absent, got)
				}
			}
			if len(got) > upNextStepsLimit {
				t.Fatalf("block must stay within %d lines, got %d: %v", upNextStepsLimit, len(got), got)
			}
		})
	}
}

func TestPrintUpNextStepHintsFormatting(t *testing.T) {
	var buf bytes.Buffer // non-TTY writer: agents and scripts must still see it
	hints := buildUpNextSteps(true, false, false, false, 3)
	printUpNextStepHints(&buf, hints)
	got := buf.String()
	// Rendered as bullets so they read as part of the ready card's existing
	// "next:" list, not as a competing block.
	if !strings.HasPrefix(got, "- ") {
		t.Fatalf("hints must render as card bullets, got:\n%s", got)
	}
	if !strings.Contains(got, "okdev sync wait / exec --require-sync — block until local edits have reached the pods") {
		t.Fatalf("hints must carry both the primitive and why, got:\n%s", got)
	}
	if lines := strings.Count(got, "\n"); lines > upNextStepsLimit {
		t.Fatalf("expected at most %d hint lines, got %d:\n%s", upNextStepsLimit, lines, got)
	}

	var empty bytes.Buffer
	printUpNextStepHints(&empty, nil)
	if empty.Len() != 0 {
		t.Fatalf("no hints must print nothing, got %q", empty.String())
	}
}
