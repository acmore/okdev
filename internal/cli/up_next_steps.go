package cli

import (
	"fmt"
	"io"
	"strings"
)

// upNextStepsLimit caps how many primitives are appended to the ready card's
// existing "next:" list. Past a handful of lines a hint block reads as
// boilerplate and gets skipped wholesale, which defeats the point — and the
// card already carries three session-lifecycle entries of its own.
const upNextStepsLimit = 4

type nextStepHint struct {
	// Subject is the primitive — a command or a config key.
	Subject string
	// Why says what it does in the reader's terms, not okdev's.
	Why string
}

// buildUpNextSteps names the primitives this particular session is most likely
// to need next.
//
// The problem it addresses (#219) is not missing documentation — the skill and
// `--help` cover all of these — it is that documentation goes unread. An
// operator drove a full day of multi-node work while hand-rolling a grep loop
// to detect sync completion (three false positives, one wasted multi-GPU run),
// re-implementing a postSync hook as a shell script re-run after every
// recreate, and resolving pod IPs with `getent hosts`. Every one of those
// primitives already existed. What changes behaviour is output on the
// execution path.
//
// Which is also why the block is shape-aware rather than fixed: hints that do
// not apply to the session in front of the reader are what turn a block like
// this into noise to be ignored.
func buildUpNextSteps(hasSync, hasPostSync, hasPostCreate, waitHooksUsed bool, podCount int) []nextStepHint {
	multiPod := podCount > 1
	hints := make([]nextStepHint, 0, upNextStepsLimit)

	if hasSync {
		// The single most expensive gap: a successful `okdev sync` does not
		// mean the latest edit reached the pod, so anything that launches work
		// straight after it can run stale code. Both primitives share one line
		// because they answer the same question — the budget is better spent
		// on a hint the reader would not otherwise reach.
		hints = append(hints, nextStepHint{"okdev sync wait / exec --require-sync", "block until local edits have reached the pods, or refuse to launch until they have"})
	}
	switch {
	case !hasPostSync && !hasPostCreate:
		hints = append(hints, nextStepHint{"spec.lifecycle.postCreate / postSync", "re-run setup automatically on every recreate; `okdev env-diff` drafts it from pod-local drift"})
	case multiPod && !waitHooksUsed:
		hints = append(hints, nextStepHint{"okdev up --wait-hooks", "converge hooks on every pod, including controller-created late arrivals"})
	}
	if multiPod {
		hints = append(hints, nextStepHint{"okdev exec --all | --role worker", "fanout is opt-in; a selector-less exec runs on the target pod only"})
		hints = append(hints, nextStepHint{"master-0 / worker-1", "short-name aliases that resolve inside every pod, so launch scripts need no pod-IP lookups"})
	}
	hints = append(hints, nextStepHint{"okdev jobs logs --tail --grep", "follow detached runs without re-deriving pod names"})

	if len(hints) > upNextStepsLimit {
		hints = hints[:upNextStepsLimit]
	}
	return hints
}

// printUpNextStepHints appends the hints to the ready card's "next:" list.
// They join the existing entries rather than opening a second "next:" section:
// two blocks under the same heading is how a reader learns to skip both.
//
// Not TTY-gated: agents and scripts are the callers that operate from memory
// of the tool and never read the skill, and `up` runs once per session or
// recreate, so the cost is bounded.
func printUpNextStepHints(w io.Writer, hints []nextStepHint) {
	if w == nil {
		return
	}
	for _, hint := range hints {
		fmt.Fprintf(w, "- %s — %s\n", hint.Subject, strings.TrimSpace(hint.Why))
	}
}
