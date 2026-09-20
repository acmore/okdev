package kube

import (
	"context"
	"encoding/json"
	"sort"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

const PodGroupAPIVersion = "scheduling.volcano.sh/v1beta1"

type PodGroupCondition struct {
	Type               string `json:"type"`
	Status             string `json:"status"`
	Reason             string `json:"reason,omitempty"`
	Message            string `json:"message,omitempty"`
	LastTransitionTime string `json:"lastTransitionTime,omitempty"`
}

type PodGroupEvent struct {
	Type     string `json:"type"`
	Reason   string `json:"reason"`
	Message  string `json:"message"`
	Count    int32  `json:"count"`
	LastSeen string `json:"lastSeen,omitempty"`
}

type PodGroupDiagnostics struct {
	Name        string              `json:"name"`
	APIVersion  string              `json:"apiVersion"`
	UID         string              `json:"uid,omitempty"`
	Queue       string              `json:"queue,omitempty"`
	Phase       string              `json:"phase,omitempty"`
	MinMember   int64               `json:"minMember,omitempty"`
	Conditions  []PodGroupCondition `json:"conditions,omitempty"`
	Events      []PodGroupEvent     `json:"events,omitempty"`
	EventsError string              `json:"eventsError,omitempty"`
}

// Read the known namespaced API directly: ordinary status needs neither CRD
// discovery permissions nor a dependency on the Volcano Go client.
func (c *Client) GetPodGroupDiagnostics(ctx context.Context, namespace, name string) (*PodGroupDiagnostics, error) {
	cs, dc, _, _, err := c.clients()
	if err != nil {
		return nil, err
	}
	obj, err := dc.Resource(schema.GroupVersionResource{Group: "scheduling.volcano.sh", Version: "v1beta1", Resource: "podgroups"}).Namespace(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	var value struct {
		Spec struct {
			Queue     string `json:"queue"`
			MinMember int64  `json:"minMember"`
		} `json:"spec"`
		Status struct {
			Phase      string              `json:"phase"`
			Conditions []PodGroupCondition `json:"conditions"`
		} `json:"status"`
	}
	data, err := json.Marshal(obj.Object)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	result := &PodGroupDiagnostics{Name: name, APIVersion: PodGroupAPIVersion, UID: string(obj.GetUID()), Queue: value.Spec.Queue, MinMember: value.Spec.MinMember, Phase: value.Status.Phase, Conditions: value.Status.Conditions}
	selector := fields.Set{"involvedObject.kind": "PodGroup", "involvedObject.name": name, "involvedObject.uid": result.UID}
	events, err := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: selector.AsSelector().String()})
	if err != nil {
		result.EventsError = err.Error()
		return result, nil
	}
	sort.SliceStable(events.Items, func(i, j int) bool {
		return podGroupEventTime(events.Items[i]).Before(podGroupEventTime(events.Items[j]))
	})
	for _, event := range events.Items {
		// Also filter client-side so a same-name Pod or a previous PodGroup
		// incarnation cannot supply misleading scheduling evidence.
		if event.InvolvedObject.Kind != "PodGroup" || event.InvolvedObject.UID != obj.GetUID() || event.InvolvedObject.Name != name {
			continue
		}
		at := podGroupEventTime(event)
		lastSeen := ""
		if !at.IsZero() {
			lastSeen = at.UTC().Format(time.RFC3339)
		}
		count := event.Count
		if event.Series != nil {
			count = max(count, event.Series.Count)
		}
		result.Events = append(result.Events, PodGroupEvent{Type: event.Type, Reason: event.Reason, Message: event.Message, Count: count, LastSeen: lastSeen})
	}
	if len(result.Events) > 10 {
		result.Events = result.Events[len(result.Events)-10:]
	}
	return result, nil
}

func podGroupEventTime(event corev1.Event) time.Time {
	if event.Series != nil && !event.Series.LastObservedTime.IsZero() {
		return event.Series.LastObservedTime.Time
	}
	if !event.LastTimestamp.IsZero() {
		return event.LastTimestamp.Time
	}
	if !event.EventTime.IsZero() {
		return event.EventTime.Time
	}
	return event.CreationTimestamp.Time
}
