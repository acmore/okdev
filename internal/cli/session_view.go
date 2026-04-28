package cli

import (
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
)

type sessionView struct {
	Namespace    string
	Session      string
	Owner        string
	WorkloadType string
	TargetPod    string
	Phase        string
	Ready        string
	Restarts     int32
	Reason       string
	CreatedAt    time.Time
	PodCount     int
	Pods         []kube.PodSummary
}

func buildSessionViews(pods []kube.PodSummary) []sessionView {
	grouped := map[string][]kube.PodSummary{}
	for _, pod := range pods {
		sessionName := sessionNameFromPodSummary(pod)
		key := strings.Join([]string{pod.Namespace, pod.Labels["okdev.io/owner"], sessionName}, "\x00")
		grouped[key] = append(grouped[key], pod)
	}

	views := make([]sessionView, 0, len(grouped))
	for _, group := range grouped {
		if len(group) == 0 {
			continue
		}
		sort.Slice(group, func(i, j int) bool {
			return workload.ComparePodPriority(group[i], group[j])
		})
		target, ok := selectTargetPod(group)
		if !ok {
			continue
		}
		summary := target
		oldest := group[0].CreatedAt
		for _, pod := range group[1:] {
			if pod.CreatedAt.Before(oldest) {
				oldest = pod.CreatedAt
			}
		}
		views = append(views, sessionView{
			Namespace:    summary.Namespace,
			Session:      sessionNameFromPodSummary(summary),
			Owner:        summary.Labels["okdev.io/owner"],
			WorkloadType: summary.Labels["okdev.io/workload-type"],
			TargetPod:    target.Name,
			Phase:        summary.Phase,
			Ready:        summary.Ready,
			Restarts:     summary.Restarts,
			Reason:       summary.Reason,
			CreatedAt:    oldest,
			PodCount:     len(group),
			Pods:         group,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	return views
}

func buildControllerSessionViews(resources []kube.ResourceSummary) []sessionView {
	views := make([]sessionView, 0, len(resources))
	for _, resource := range resources {
		sessionName := strings.TrimSpace(resource.Labels["okdev.io/session"])
		if sessionName == "" {
			continue
		}
		workloadType := strings.TrimSpace(resource.Labels["okdev.io/workload-type"])
		if workloadType == "" {
			workloadType = strings.ToLower(strings.TrimSpace(resource.Kind))
		}
		views = append(views, sessionView{
			Namespace:    resource.Namespace,
			Session:      sessionName,
			Owner:        resource.Labels["okdev.io/owner"],
			WorkloadType: workloadType,
			Phase:        "Pending",
			Ready:        "0/0",
			Reason:       "waiting for workload pods",
			CreatedAt:    resource.CreatedAt,
			PodCount:     0,
		})
	}
	sort.Slice(views, func(i, j int) bool {
		return views[i].CreatedAt.After(views[j].CreatedAt)
	})
	return views
}

func selectTargetPod(pods []kube.PodSummary) (kube.PodSummary, bool) {
	if len(pods) == 0 {
		return kube.PodSummary{}, false
	}
	var chosen *kube.PodSummary
	var chosenAttach time.Time
	for i := range pods {
		ts := strings.TrimSpace(pods[i].Annotations["okdev.io/last-attach"])
		if ts == "" {
			continue
		}
		parsed, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			continue
		}
		if chosen == nil || parsed.After(chosenAttach) {
			chosen = &pods[i]
			chosenAttach = parsed
		}
	}
	if chosen != nil {
		return *chosen, true
	}
	return pods[0], true
}
