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

// captureSessionLastSeen persists a live pod snapshot for every viewed
// session that okdev tracks locally (has session.json). Best-effort: status
// output must not fail because a cache write did.
func captureSessionLastSeen(views []sessionView) {
	for _, view := range views {
		if len(view.Pods) == 0 {
			continue
		}
		info, err := session.LoadInfo(view.Session)
		if err != nil || strings.TrimSpace(info.Name) == "" {
			continue
		}
		snapshot := session.LastSeen{
			At:        time.Now().UTC(),
			Namespace: view.Namespace,
			Workload: session.LastSeenWorkload{
				APIVersion: info.WorkloadAPIVersion,
				Kind:       info.WorkloadKind,
				Name:       info.WorkloadName,
			},
		}
		for _, pod := range view.Pods {
			entry := session.LastSeenPod{
				Name:     pod.Name,
				Phase:    pod.Phase,
				Ready:    pod.Ready,
				Reason:   pod.Reason,
				Deleting: pod.Deleting,
				Node:     pod.NodeName,
			}
			for _, issue := range pod.ContainerIssues {
				entry.Issues = append(entry.Issues, session.LastSeenPodIssue{
					Container: issue.Container,
					Reason:    issue.Reason,
					ExitCode:  issue.ExitCode,
				})
			}
			snapshot.Pods = append(snapshot.Pods, entry)
		}
		if err := session.SaveLastSeen(view.Session, snapshot); err != nil {
			slog.Debug("failed to save last-seen snapshot", "session", view.Session, "error", err)
		}
	}
}

// sessionDeathReport is what okdev can still say about a session whose
// cluster objects are gone: the cached last-seen snapshot plus any events
// that outlived the objects. It distinguishes "reclaimed, safe to recreate"
// from "evicted for quota/node reasons, recreating won't help".
type sessionDeathReport struct {
	Session    string                   `json:"session"`
	Found      bool                     `json:"found"`
	LastSeenAt string                   `json:"lastSeenAt,omitempty"`
	Workload   session.LastSeenWorkload `json:"workload,omitempty"`
	Pods       []session.LastSeenPod    `json:"pods,omitempty"`
	Events     []deathReportEvent       `json:"events,omitempty"`
}

type deathReportEvent struct {
	Object   string `json:"object"`
	Type     string `json:"type,omitempty"`
	Reason   string `json:"reason"`
	Message  string `json:"message,omitempty"`
	LastSeen string `json:"lastSeen,omitempty"`
}

type objectEventLister interface {
	ListObjectEvents(ctx context.Context, namespace, name string) ([]kube.EventSummary, error)
}

const deathReportMaxEvents = 20

// buildSessionDeathReport assembles the post-mortem for a vanished session.
// Returns ok=false when there is nothing cached to report (no local session
// or no snapshot) — the caller then keeps the plain not-found message.
func buildSessionDeathReport(ctx context.Context, k objectEventLister, sessionName, namespace string) (sessionDeathReport, bool) {
	snapshot, err := session.LoadLastSeen(sessionName)
	if err != nil || snapshot.At.IsZero() {
		return sessionDeathReport{}, false
	}
	report := sessionDeathReport{
		Session:    sessionName,
		Found:      false,
		LastSeenAt: snapshot.At.UTC().Format(time.RFC3339),
		Workload:   snapshot.Workload,
		Pods:       snapshot.Pods,
	}
	eventNamespace := strings.TrimSpace(snapshot.Namespace)
	if eventNamespace == "" {
		eventNamespace = namespace
	}
	names := make([]string, 0, len(snapshot.Pods)+1)
	if strings.TrimSpace(snapshot.Workload.Name) != "" {
		names = append(names, snapshot.Workload.Name)
	}
	for _, pod := range snapshot.Pods {
		names = append(names, pod.Name)
	}
	for _, name := range names {
		events, err := k.ListObjectEvents(ctx, eventNamespace, name)
		if err != nil {
			slog.Debug("failed to list post-mortem events", "object", name, "error", err)
			continue
		}
		for _, event := range events {
			entry := deathReportEvent{
				Object:  event.InvolvedName,
				Type:    event.Type,
				Reason:  event.Reason,
				Message: event.Message,
			}
			if !event.LastSeen.IsZero() {
				entry.LastSeen = event.LastSeen.UTC().Format(time.RFC3339)
			}
			report.Events = append(report.Events, entry)
		}
	}
	if len(report.Events) > deathReportMaxEvents {
		report.Events = report.Events[len(report.Events)-deathReportMaxEvents:]
	}
	return report, true
}

func printSessionDeathReport(w io.Writer, report sessionDeathReport) {
	fmt.Fprintf(w, "\nLast known state before the session disappeared (captured %s):\n", report.LastSeenAt)
	if strings.TrimSpace(report.Workload.Name) != "" {
		fmt.Fprintf(w, "- workload: %s/%s (deleted)\n", report.Workload.Kind, report.Workload.Name)
	}
	for _, pod := range report.Pods {
		line := fmt.Sprintf("- pod %s: %s", pod.Name, pod.Phase)
		if pod.Reason != "" && pod.Reason != "-" {
			line += " (" + pod.Reason + ")"
		}
		if pod.Deleting {
			line += " [terminating]"
		}
		fmt.Fprintln(w, line)
		for _, issue := range pod.Issues {
			fmt.Fprintf(w, "  container %s: %s (exit %d)\n", issue.Container, issue.Reason, issue.ExitCode)
		}
	}
	if len(report.Events) > 0 {
		fmt.Fprintln(w, "Recent events for those objects:")
		for _, event := range report.Events {
			line := fmt.Sprintf("- [%s] %s %s", event.Reason, event.Object, event.Message)
			fmt.Fprintln(w, strings.TrimRight(line, " "))
		}
	} else {
		fmt.Fprintln(w, "No cluster events survived for those objects (they expire ~1h after emission).")
	}
	fmt.Fprintln(w, "If the events show Evicted/Preempted or quota errors, recreating will likely fail again; otherwise `okdev up` recreates the session.")
}
