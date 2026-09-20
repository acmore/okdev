package cli

import (
	"context"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type podGroupStatusClient interface {
	GetPodGroupDiagnostics(context.Context, string, string) (*kube.PodGroupDiagnostics, error)
}

type detailedStatusPodGroup struct {
	kube.PodGroupDiagnostics
	Pods        []string `json:"pods"`
	LookupError string   `json:"lookupError,omitempty"`
}

func gatherPodGroupStatus(ctx context.Context, namespace string, pods []kube.PodSummary, client statusDetailsClient) []detailedStatusPodGroup {
	reader, ok := client.(podGroupStatusClient)
	if !ok {
		return nil
	}
	members := map[string][]string{}
	for _, pod := range pods {
		name := strings.TrimSpace(pod.Annotations["scheduling.volcano.sh/group-name"])
		if name == "" {
			name = strings.TrimSpace(pod.Annotations["scheduling.k8s.io/group-name"])
		}
		if name != "" {
			members[name] = append(members[name], pod.Name)
		}
	}
	var names []string
	for name := range members {
		names = append(names, name)
	}
	sort.Strings(names)
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var result []detailedStatusPodGroup
	for _, name := range names {
		sort.Strings(members[name])
		item := detailedStatusPodGroup{PodGroupDiagnostics: kube.PodGroupDiagnostics{Name: name, APIVersion: kube.PodGroupAPIVersion}, Pods: members[name]}
		diagnostics, err := reader.GetPodGroupDiagnostics(ctx, namespace, name)
		if err != nil {
			item.LookupError = err.Error()
		} else if diagnostics != nil {
			item.PodGroupDiagnostics = *diagnostics
		}
		result = append(result, item)
	}
	return result
}

func printPodGroupStatus(w io.Writer, groups []detailedStatusPodGroup) {
	if len(groups) == 0 {
		return
	}
	fmt.Fprintln(w, "\nPodGroups (scheduler-reported evidence):")
	for _, group := range groups {
		fmt.Fprintf(w, "- %s (%s) pods=%s\n", group.Name, group.APIVersion, strings.Join(group.Pods, ","))
		if group.LookupError != "" {
			fmt.Fprintf(w, "  details unavailable: %s\n", group.LookupError)
			continue
		}
		fmt.Fprintf(w, "  queue=%s phase=%s minMember=%d\n", emptyDash(group.Queue), emptyDash(group.Phase), group.MinMember)
		for _, condition := range group.Conditions {
			fmt.Fprintf(w, "  condition %s=%s reason=%s at=%s: %s\n", condition.Type, condition.Status, emptyDash(condition.Reason), emptyDash(condition.LastTransitionTime), condition.Message)
		}
		for _, event := range group.Events {
			fmt.Fprintf(w, "  event %s %s count=%d at=%s: %s\n", event.Type, event.Reason, event.Count, emptyDash(event.LastSeen), event.Message)
		}
		if group.EventsError != "" {
			fmt.Fprintf(w, "  events unavailable: %s\n", group.EventsError)
		}
	}
}
