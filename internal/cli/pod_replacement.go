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

// detectPodReplacement answers "which pods of the live run ended, and why?"
// by comparing the cached snapshot against the pods live now (#325). The
// run-level RunEnd stays silent here by design — the run-id is unchanged — so
// without this a controller that recreated an evicted or preempted pod leaves
// a Pending replacement and no trace of the pod that ran.
//
// A snapshot pod has ended when its name is no longer live, or is live under
// a different UID (same-name recreation). Pods last seen Succeeded finished
// on their own, and pods of a pod-template revision other than the newest
// live pod's were retired by a rollout (`up --reconcile`, `kubectl rollout
// restart`); neither is reported. Zero live pods is the session-death path
// (buildSessionDeathReport), not this one.
func detectPodReplacement(ctx context.Context, k objectEventLister, namespace string, snapshot session.LastSeen, currentRunID string, live []kube.PodSummary) (session.PodReplacement, bool) {
	runID := strings.TrimSpace(snapshot.RunID)
	if snapshot.At.IsZero() || runID == "" || runID != strings.TrimSpace(currentRunID) || len(live) == 0 {
		return session.PodReplacement{}, false
	}
	liveUIDs := make(map[string]string, len(live))
	var newest kube.PodSummary
	for _, pod := range live {
		liveUIDs[pod.Name] = pod.UID
		if newest.Name == "" || pod.CreatedAt.After(newest.CreatedAt) {
			newest = pod
		}
	}
	currentRevision := podTemplateRevision(newest.Labels)
	samePod := func(snapshotUID, liveUID string) bool {
		return snapshotUID == "" || liveUID == "" || snapshotUID == liveUID
	}
	var ended []session.LastSeenPod
	snapshotUIDs := make(map[string]string, len(snapshot.Pods))
	for _, pod := range snapshot.Pods {
		snapshotUIDs[pod.Name] = pod.UID
		if liveUID, ok := liveUIDs[pod.Name]; ok && samePod(pod.UID, liveUID) {
			continue
		}
		if strings.EqualFold(pod.Phase, "Succeeded") {
			continue
		}
		if pod.Revision != "" && currentRevision != "" && pod.Revision != currentRevision {
			continue
		}
		ended = append(ended, pod)
	}
	if len(ended) == 0 {
		return session.PodReplacement{}, false
	}
	record := session.PodReplacement{
		RunID:      runID,
		LastSeenAt: snapshot.At.UTC(),
		DetectedAt: time.Now().UTC(),
	}
	for _, pod := range ended {
		record.Ended = append(record.Ended, session.ReplacedPod{Name: pod.Name, UID: pod.UID, Node: pod.Node, Phase: pod.Phase})
	}
	for _, pod := range live {
		if snapshotUID, ok := snapshotUIDs[pod.Name]; ok && samePod(snapshotUID, pod.UID) {
			continue
		}
		record.ReplacedBy = append(record.ReplacedBy, pod.Name)
	}
	eventNamespace := strings.TrimSpace(snapshot.Namespace)
	if eventNamespace == "" {
		eventNamespace = namespace
	}
	// Only the ended pods' own events: the workload's events describe the
	// controller doing its job (creating the replacement), not the cause.
	class, _, evidence := explainRunEnd(ctx, k, eventNamespace, session.LastSeen{Pods: ended})
	record.Class, record.Reason, record.Evidence = class, podReplacementReason(class), evidence
	return record, true
}

// podTemplateRevision is the revision label Deployments (via ReplicaSets)
// and StatefulSets stamp on their pods; other controllers set none.
func podTemplateRevision(labels map[string]string) string {
	if revision := strings.TrimSpace(labels["pod-template-hash"]); revision != "" {
		return revision
	}
	return strings.TrimSpace(labels["controller-revision-hash"])
}

func podReplacementReason(class string) string {
	switch class {
	case session.RunEndClassDeleted:
		return "pod deleted outside okdev (manual delete, node drain, or controller scale-down)"
	case session.RunEndClassUnknown:
		return "cause unknown (no termination reason cached and no surviving events — cluster events expire ~1h)"
	}
	return runEndReasonForClass(class)
}

func podReplacementMitigation(class string) string {
	if class == session.RunEndClassUnknown {
		return "pod events expire ~1h; the next `okdev status` or `okdev exec` after a pod change captures them"
	}
	return runEndMitigation(class)
}

// sameEndedPods reports whether two records describe the same ended pods, so
// a repeat observation never replaces evidence with a later, emptier look.
func sameEndedPods(a, b session.PodReplacement) bool {
	if a.RunID != b.RunID || len(a.Ended) != len(b.Ended) {
		return false
	}
	for i := range a.Ended {
		if a.Ended[i].Name != b.Ended[i].Name || a.Ended[i].UID != b.Ended[i].UID {
			return false
		}
	}
	return true
}

// observeSessionPods is where a fresh pod list meets the cached snapshot. It
// records any in-run pod replacement before refreshing the snapshot, because
// the snapshot is the only local evidence of the ended pods. It returns the
// record only when it was newly detected.
func observeSessionPods(ctx context.Context, k objectEventLister, info session.Info, namespace string, pods []kube.PodSummary, kubeContext string) (session.PodReplacement, bool) {
	var detected session.PodReplacement
	newly := false
	if previous, err := session.LoadLastSeen(info.Name); err == nil {
		if record, ok := detectPodReplacement(ctx, k, namespace, previous, info.RunID, pods); ok {
			existing, _ := session.LoadPodReplacement(info.Name)
			if !sameEndedPods(existing, record) {
				if err := session.SavePodReplacement(info.Name, record); err != nil {
					slog.Debug("failed to save pod-replacement record", "session", info.Name, "error", err)
				}
				detected, newly = record, true
			}
		}
	}
	snapshot := buildLastSeenSnapshot(info, namespace, pods)
	if kubeContext != "" {
		snapshot.Context = kubeContext
	}
	if err := session.SaveLastSeen(info.Name, snapshot); err != nil {
		slog.Debug("failed to save last-seen snapshot", "session", info.Name, "error", err)
	}
	return detected, newly
}

// currentSessionPodReplacement returns the stored record while it describes
// the run that is live now.
func currentSessionPodReplacement(sessionName string) (session.PodReplacement, bool) {
	info, err := session.LoadInfo(sessionName)
	if err != nil || strings.TrimSpace(info.RunID) == "" {
		return session.PodReplacement{}, false
	}
	record, err := session.LoadPodReplacement(sessionName)
	if err != nil || len(record.Ended) == 0 || record.RunID != strings.TrimSpace(info.RunID) {
		return session.PodReplacement{}, false
	}
	return record, true
}

func formatPodReplacement(record session.PodReplacement) string {
	names := make([]string, 0, len(record.Ended))
	nodes := make([]string, 0, len(record.Ended))
	for _, pod := range record.Ended {
		names = append(names, pod.Name)
		if node := strings.TrimSpace(pod.Node); node != "" {
			nodes = append(nodes, node)
		}
	}
	subject := "pod " + strings.Join(names, ", ")
	if len(names) > 1 {
		subject = "pods " + strings.Join(names, ", ")
	}
	if len(record.ReplacedBy) > 0 {
		subject += " replaced by " + strings.Join(record.ReplacedBy, ", ")
	} else {
		subject += " ended (not replaced)"
	}
	line := fmt.Sprintf("%s: %s", subject, record.Reason)
	if evidence := strings.TrimSpace(record.Evidence); evidence != "" {
		line += " [" + evidence + "]"
	}
	if !record.LastSeenAt.IsZero() {
		line += fmt.Sprintf("; last seen alive %s", record.LastSeenAt.UTC().Format(time.RFC3339))
		if len(nodes) > 0 {
			line += " on node " + strings.Join(nodes, ", ")
		}
	}
	return line
}

func printPodReplacement(w io.Writer, record session.PodReplacement) {
	if w == nil {
		return
	}
	fmt.Fprintln(w, formatPodReplacement(record))
	if mitigation := podReplacementMitigation(record.Class); mitigation != "" {
		fmt.Fprintf(w, "  %s\n", mitigation)
	}
}

// reportExecPodReplacement runs the same observation on exec's pod list,
// which is already in hand: exec is where a replaced pod is usually first
// noticed, and its events expire long before a later status would look.
func reportExecPodReplacement(ctx context.Context, cc *commandContext, pods []kube.PodSummary, warnOut io.Writer) {
	if cc == nil || cc.kube == nil || (cc.cfg != nil && cc.cfg.Spec.AttachOnly != nil) || strings.TrimSpace(cc.sessionName) == "" || len(pods) == 0 {
		return
	}
	info, err := session.LoadInfo(cc.sessionName)
	if err != nil || strings.TrimSpace(info.Name) == "" {
		return
	}
	kubeContext := ""
	if cc.opts != nil {
		kubeContext = cc.opts.Context
	}
	if !statusScopeMatches(info.Namespace, info.KubeContext, cc.namespace, []string{kubeContext}) {
		return
	}
	if record, ok := observeSessionPods(ctx, cc.kube, info, cc.namespace, pods, kubeContext); ok {
		fmt.Fprintf(warnOut, "notice: %s\n", formatPodReplacement(record))
	}
}
