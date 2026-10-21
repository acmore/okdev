package cli

import (
	"fmt"
	"io"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func targetPodNode(view sessionView) string {
	for _, pod := range view.Pods {
		if pod.Name == view.TargetPod {
			return pod.NodeName
		}
	}
	return ""
}

// nodeChangesSince is the node-change record for the next snapshot: changes
// the previous snapshot retained for pods that are still live, plus pods
// that now run on a different node than the previous snapshot saw. It is
// computed wherever a snapshot is refreshed (status and exec), so a refresh
// never erases the only evidence that a pod moved.
func nodeChangesSince(previous session.LastSeen, pods []kube.PodSummary) map[string]string {
	changes := make(map[string]string)
	for _, pod := range pods {
		if oldNode := previous.NodeChanges[pod.Name]; oldNode != "" {
			changes[pod.Name] = oldNode
		}
	}
	byName := make(map[string]string, len(previous.Pods))
	for _, pod := range previous.Pods {
		byName[pod.Name] = pod.Node
	}
	for _, pod := range pods {
		oldNode := byName[pod.Name]
		if oldNode == "" && len(previous.Pods) == 1 && len(pods) == 1 {
			oldNode = previous.Pods[0].Node
		}
		if strings.TrimSpace(oldNode) != "" && strings.TrimSpace(pod.NodeName) != "" && oldNode != pod.NodeName {
			if changes[pod.Name] == "" {
				changes[pod.Name] = oldNode
			}
		}
	}
	if len(changes) == 0 {
		return nil
	}
	return changes
}

// statusNodeChanges returns the node changes retained in the session's
// snapshot for the live pods of this run. Status refreshes the snapshot
// (observeSessionPods) before calling it.
func statusNodeChanges(view sessionView, contextName string) map[string]string {
	changes := make(map[string]string)
	info, err := session.LoadInfo(view.Session)
	if err != nil || info.Name == "" || !statusScopeMatches(info.Namespace, info.KubeContext, view.Namespace, []string{contextName}) ||
		(info.Owner != "" && view.Owner != "" && info.Owner != view.Owner) {
		return changes
	}
	snapshot, err := session.LoadLastSeen(view.Session)
	if err != nil || snapshot.At.IsZero() || !statusScopeMatches(snapshot.Namespace, snapshot.Context, view.Namespace, []string{contextName}) ||
		(snapshot.RunID != "" && info.RunID != "" && snapshot.RunID != info.RunID) {
		return changes
	}
	for _, pod := range view.Pods {
		if oldNode := snapshot.NodeChanges[pod.Name]; oldNode != "" {
			changes[pod.Name] = oldNode
		}
	}
	return changes
}

func printStatusNodeChanges(w io.Writer, view sessionView, changes map[string]string) {
	for _, pod := range view.Pods {
		if oldNode := changes[pod.Name]; oldNode != "" {
			fmt.Fprintf(w, "warning: pod %s moved from node %s to %s; emptyDir data is gone and hostPath data may be unavailable\n", pod.Name, oldNode, pod.NodeName)
		}
	}
}
