package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

// detectPreviousRunEnd answers "did the run I am replacing end, and why?" by
// comparing the cached last-seen snapshot's run identity against the run that
// is starting now. It is deliberately narrow: intentional teardowns (`down`,
// `restart`) drop the snapshot, so a surviving snapshot from a different run
// always describes a run that ended without being asked to.
//
// Returns ok=false when there is nothing to report — no snapshot, or a
// snapshot describing the very run that is starting (a reused or reconciled
// workload keeps its run-id, so those stay silent).
func detectPreviousRunEnd(ctx context.Context, k objectEventLister, sessionName, namespace, currentRunID, currentWorkloadName string) (session.RunEnd, bool) {
	snapshot, err := session.LoadLastSeen(sessionName)
	if err != nil || snapshot.At.IsZero() {
		return session.RunEnd{}, false
	}
	endedRunID := strings.TrimSpace(snapshot.RunID)
	endedWorkload := strings.TrimSpace(snapshot.Workload.Name)
	switch {
	case endedRunID != "":
		if endedRunID == strings.TrimSpace(currentRunID) {
			return session.RunEnd{}, false
		}
	case endedWorkload != "" && endedWorkload != strings.TrimSpace(currentWorkloadName):
		// Snapshot written before run-ids were recorded: the workload name
		// carries the run suffix, so a different name still proves a different
		// run. Reported without an id rather than not at all.
	default:
		return session.RunEnd{}, false
	}

	record := session.RunEnd{
		EndedRunID:       endedRunID,
		EndedWorkload:    endedWorkload,
		LastSeenAt:       snapshot.At.UTC(),
		DetectedAt:       time.Now().UTC(),
		SucceededByRunID: strings.TrimSpace(currentRunID),
	}
	eventNamespace := strings.TrimSpace(snapshot.Namespace)
	if eventNamespace == "" {
		eventNamespace = namespace
	}
	record.Class, record.Reason, record.Evidence = explainRunEnd(ctx, k, eventNamespace, snapshot)
	return record, true
}

// explainRunEnd picks the best available cause of death. Surviving cluster
// events win over the cached pod states: the snapshot was taken the last time
// status ran, which may be long before the run died, whereas an event is
// recorded at the moment it happened.
func explainRunEnd(ctx context.Context, k objectEventLister, namespace string, snapshot session.LastSeen) (class, reason, evidence string) {
	names := make([]string, 0, len(snapshot.Pods)+1)
	if strings.TrimSpace(snapshot.Workload.Name) != "" {
		names = append(names, snapshot.Workload.Name)
	}
	for _, pod := range snapshot.Pods {
		names = append(names, pod.Name)
	}
	if k != nil {
		best := ""
		for _, name := range names {
			events, err := k.ListObjectEvents(ctx, namespace, name)
			if err != nil {
				slog.Debug("failed to list events for ended run", "object", name, "error", err)
				continue
			}
			for _, event := range events {
				eventClass := classifyRunEndEvent(event.Reason, event.Message)
				if eventClass == session.RunEndClassUnknown {
					continue
				}
				// A specific cause outranks a bare deletion: the operator
				// deleting the pod is the mechanism, not the reason.
				if best != "" && eventClass == session.RunEndClassDeleted {
					continue
				}
				best = eventClass
				evidence = formatRunEndEventEvidence(event)
				if eventClass != session.RunEndClassDeleted {
					break
				}
			}
			if best != "" && best != session.RunEndClassDeleted {
				break
			}
		}
		if best != "" {
			return best, runEndReasonForClass(best), evidence
		}
	}

	// No event survived (they expire ~1h) — fall back to whatever terminal
	// detail the snapshot itself captured.
	for _, pod := range snapshot.Pods {
		for _, issue := range pod.Issues {
			if issueClass := classifyRunEndEvent(issue.Reason, ""); issueClass != session.RunEndClassUnknown {
				return issueClass, runEndReasonForClass(issueClass),
					fmt.Sprintf("cached container %s in pod %s: %s (exit %d)", issue.Container, pod.Name, issue.Reason, issue.ExitCode)
			}
			if issue.ExitCode != 0 {
				return session.RunEndClassFailed, runEndReasonForClass(session.RunEndClassFailed),
					fmt.Sprintf("cached container %s in pod %s: %s (exit %d)", issue.Container, pod.Name, issue.Reason, issue.ExitCode)
			}
		}
		if podClass := classifyRunEndEvent(pod.Reason, ""); podClass != session.RunEndClassUnknown {
			return podClass, runEndReasonForClass(podClass),
				fmt.Sprintf("cached pod %s: %s (%s)", pod.Name, pod.Phase, pod.Reason)
		}
	}
	return session.RunEndClassUnknown, runEndReasonForClass(session.RunEndClassUnknown), ""
}

// classifyRunEndEvent maps a Kubernetes event reason/message onto a run-end
// class. Matching is on both fields because reclaim controllers are not
// standardized: some emit a custom reason, others a generic one with the real
// story in the message.
func classifyRunEndEvent(reason, message string) string {
	// Startup-side events are dropped before any keyword matching. Their
	// messages are full of words that look terminal — a FailedScheduling
	// message reading "Insufficient nvidia.com/gpu" would otherwise be
	// classified as a preemption and send the reader after the wrong fix.
	switch strings.ToLower(strings.TrimSpace(reason)) {
	case "scheduled", "failedscheduling", "pulling", "pulled", "pulledimage",
		"created", "started", "successfulcreate", "failedmount", "backoff",
		"failedattachvolume", "notriggerscaleup", "triggeredscaleup":
		return session.RunEndClassUnknown
	}
	text := strings.ToLower(strings.TrimSpace(reason + " " + message))
	if strings.TrimSpace(text) == "" {
		return session.RunEndClassUnknown
	}
	switch {
	case strings.Contains(text, "oomkill"), strings.Contains(text, "out of memory"):
		return session.RunEndClassOOM
	case strings.Contains(text, "idle"), strings.Contains(text, "inactiv"):
		return session.RunEndClassIdleReclaim
	case strings.Contains(text, "preempt"), strings.Contains(text, "evict"),
		strings.Contains(text, "quota"), strings.Contains(text, "insufficient"),
		strings.Contains(text, "nodeshutdown"), strings.Contains(text, "disruption"):
		return session.RunEndClassEvicted
	case strings.Contains(text, "reclaim"), strings.Contains(text, "expired"),
		strings.Contains(text, "ttl"), strings.Contains(text, "timelimit"),
		strings.Contains(text, "time limit"):
		return session.RunEndClassIdleReclaim
	case strings.Contains(text, "backoff"), strings.Contains(text, "crashloop"),
		strings.Contains(text, "error"), strings.Contains(text, "failed"):
		return session.RunEndClassFailed
	case strings.Contains(text, "killing"), strings.Contains(text, "delete"),
		strings.Contains(text, "stopping"), strings.Contains(text, "terminat"):
		return session.RunEndClassDeleted
	}
	return session.RunEndClassUnknown
}

func runEndReasonForClass(class string) string {
	switch class {
	case session.RunEndClassIdleReclaim:
		return "reclaimed by the cluster (idle/TTL policy, not preemption)"
	case session.RunEndClassEvicted:
		return "evicted or preempted"
	case session.RunEndClassOOM:
		return "container OOM-killed"
	case session.RunEndClassFailed:
		return "container exited non-zero"
	case session.RunEndClassDeleted:
		return "workload deleted outside okdev"
	default:
		return "cause unknown (no termination reason cached and no surviving events — cluster events expire ~1h)"
	}
}

// runEndMitigation exists because the two most common classes call for
// opposite responses, and acting on the wrong one costs hours (#213).
func runEndMitigation(class string) string {
	switch class {
	case session.RunEndClassIdleReclaim:
		return "an idle/TTL reclaim is not preemption: keeping the workload busy (or accepting the cycle) is the fix, not chunking the run"
	case session.RunEndClassEvicted:
		return "eviction/preemption: make the run resumable and chunk long jobs; recreating into the same pressure is likely to repeat"
	case session.RunEndClassOOM:
		return "raise the container memory limit or lower the workload's peak before the next long run"
	case session.RunEndClassUnknown:
		return "run `okdev status` while the session is alive so the next end has a snapshot to explain it"
	}
	return ""
}

func formatRunEndEventEvidence(event kube.EventSummary) string {
	message := strings.TrimSpace(event.Message)
	object := strings.TrimSpace(event.InvolvedName)
	out := fmt.Sprintf("event %s", strings.TrimSpace(event.Reason))
	if object != "" {
		out += " on " + object
	}
	if message != "" {
		out += fmt.Sprintf(": %q", message)
	}
	if !event.LastSeen.IsZero() {
		out += fmt.Sprintf(" (%s)", event.LastSeen.UTC().Format(time.RFC3339))
	}
	return out
}

// previousRunEndForCurrentRun returns the stored record only while it explains
// the run that is live now. Once a third run starts the record is stale and
// stays hidden rather than following the session around forever.
func previousRunEndForCurrentRun(sessionName, currentRunID string) (session.RunEnd, bool) {
	record, err := session.LoadRunEnd(sessionName)
	if err != nil || strings.TrimSpace(record.EndedRunID)+strings.TrimSpace(record.EndedWorkload) == "" {
		return session.RunEnd{}, false
	}
	if strings.TrimSpace(record.SucceededByRunID) != strings.TrimSpace(currentRunID) {
		return session.RunEnd{}, false
	}
	return record, true
}

// currentSessionPreviousRunEnd resolves the live run from session.Info and
// returns the stored report if it explains that run.
func currentSessionPreviousRunEnd(sessionName string) (session.RunEnd, bool) {
	info, err := session.LoadInfo(sessionName)
	if err != nil || strings.TrimSpace(info.RunID) == "" {
		return session.RunEnd{}, false
	}
	return previousRunEndForCurrentRun(sessionName, info.RunID)
}

// printPreviousRunEnd renders the report on the path the user is already on.
// One line carries the fact, a second the class-specific mitigation — the
// line the reader would otherwise have to guess at.
func printPreviousRunEnd(w io.Writer, record session.RunEnd) {
	if w == nil {
		return
	}
	subject := "previous run"
	if id := strings.TrimSpace(record.EndedRunID); id != "" {
		subject += " " + id
	} else if name := strings.TrimSpace(record.EndedWorkload); name != "" {
		subject += " " + name
	}
	line := fmt.Sprintf("%s ended: %s", subject, record.Reason)
	if evidence := strings.TrimSpace(record.Evidence); evidence != "" {
		line += " [" + evidence + "]"
	}
	if !record.LastSeenAt.IsZero() {
		line += fmt.Sprintf("; last seen alive %s", record.LastSeenAt.UTC().Format(time.RFC3339))
	}
	fmt.Fprintln(w, line)
	if mitigation := runEndMitigation(record.Class); mitigation != "" {
		fmt.Fprintf(w, "  %s\n", mitigation)
	}
}
