package cli

import (
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

// Lifecycle-hook progress is tracked per pod in annotations: the legacy
// `-done` marker gates replay (older CLIs keep working), while `-state` and
// `-at` expose pending/running/done/failed to `okdev status` and let a done
// marker be recognized as stale after an in-place container restart (the
// annotations survive the restart, the container filesystem does not).
const (
	annotationPostCreateDone  = "okdev.io/post-create-done"
	annotationPostCreateState = "okdev.io/post-create-state"
	annotationPostCreateAt    = "okdev.io/post-create-at"
	annotationPostSyncDone    = "okdev.io/post-sync-done"
	annotationPostSyncState   = "okdev.io/post-sync-state"
	annotationPostSyncAt      = "okdev.io/post-sync-at"
)

const (
	hookStatePending = "pending"
	hookStateRunning = "running"
	hookStateDone    = "done"
	hookStateFailed  = "failed"
	// hookStateStale means the hook completed on a previous container
	// instance: the done marker predates the container's current start, so
	// the hook's effects (installed tools, editable installs) are gone.
	hookStateStale = "stale"
)

type hookAnnotations struct {
	done  string
	state string
	at    string
}

var (
	postCreateHook = hookAnnotations{done: annotationPostCreateDone, state: annotationPostCreateState, at: annotationPostCreateAt}
	postSyncHook   = hookAnnotations{done: annotationPostSyncDone, state: annotationPostSyncState, at: annotationPostSyncAt}
)

// computeHookState derives the pod's hook state from its annotations and the
// hook container's current start time. Pods annotated by an older CLI (done
// marker without state/timestamp) read as done — no timestamp means staleness
// cannot be established.
func computeHookState(pod kube.PodSummary, hook hookAnnotations, container string) (string, time.Time) {
	state := strings.TrimSpace(pod.Annotations[hook.state])
	var at time.Time
	if raw := strings.TrimSpace(pod.Annotations[hook.at]); raw != "" {
		if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
			at = parsed
		}
	}
	if state == "" {
		if strings.TrimSpace(pod.Annotations[hook.done]) == "true" {
			state = hookStateDone
		} else {
			return hookStatePending, at
		}
	}
	if !at.IsZero() {
		if started, ok := containerStartTime(pod, container); ok && started.After(at) {
			return hookStateStale, at
		}
	}
	return state, at
}

func containerStartTime(pod kube.PodSummary, container string) (time.Time, bool) {
	for _, start := range pod.ContainerStarts {
		if start.Name == container {
			return start.StartedAt, !start.StartedAt.IsZero()
		}
	}
	return time.Time{}, false
}

// hookNeedsRun reports whether the up path should (re)run the hook on this
// pod. Everything but a fresh done counts: pending and failed as before, and
// stale so an in-place container restart heals on the next `okdev up`
// instead of silently keeping a wiped environment. A running state is
// re-runnable too — it can only be observed here if a previous okdev died
// mid-hook (the up path runs hooks synchronously under the session lock).
func hookNeedsRun(state string) bool {
	return state != hookStateDone
}

func hookRunningAnnotations(hook hookAnnotations, now time.Time) map[string]string {
	return map[string]string{
		hook.state: hookStateRunning,
		hook.at:    now.UTC().Format(time.RFC3339),
	}
}

func hookDoneAnnotations(hook hookAnnotations, now time.Time) map[string]string {
	return map[string]string{
		hook.done:  "true",
		hook.state: hookStateDone,
		hook.at:    now.UTC().Format(time.RFC3339),
	}
}

func hookFailedAnnotations(hook hookAnnotations, now time.Time) map[string]string {
	return map[string]string{
		hook.done:  "false",
		hook.state: hookStateFailed,
		hook.at:    now.UTC().Format(time.RFC3339),
	}
}
