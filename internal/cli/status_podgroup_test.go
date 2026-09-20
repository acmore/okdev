package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type podGroupDetailsFake struct {
	fakeStatusDetailsClient
	calls []string
	err   error
}

func (f *podGroupDetailsFake) GetPodGroupDiagnostics(ctx context.Context, namespace, name string) (*kube.PodGroupDiagnostics, error) {
	deadline, ok := ctx.Deadline()
	if !ok || time.Until(deadline) > 5*time.Second {
		return nil, errors.New("optional lookup has no bounded deadline")
	}
	f.calls = append(f.calls, namespace+"/"+name)
	return &kube.PodGroupDiagnostics{Name: name, APIVersion: kube.PodGroupAPIVersion, Queue: "research", Phase: "Pending", Conditions: []kube.PodGroupCondition{{Type: "Unschedulable", Status: "True", Reason: "NotEnoughResources", Message: "scheduler did not report a specific root cause"}}}, f.err
}

func TestPodGroupStatusDeduplicatesAndPreservesEvidence(t *testing.T) {
	client := &podGroupDetailsFake{}
	pods := []kube.PodSummary{
		{Name: "b", Annotations: map[string]string{"scheduling.k8s.io/group-name": "group"}},
		{Name: "a", Annotations: map[string]string{"scheduling.volcano.sh/group-name": "group"}},
		{Name: "ordinary"},
	}
	groups := gatherPodGroupStatus(context.Background(), "demo", pods, client)
	if !reflect.DeepEqual(client.calls, []string{"demo/group"}) || len(groups) != 1 || !reflect.DeepEqual(groups[0].Pods, []string{"a", "b"}) {
		t.Fatalf("calls=%v groups=%+v", client.calls, groups)
	}
	var text bytes.Buffer
	printPodGroupStatus(&text, groups)
	if !strings.Contains(text.String(), "scheduler did not report a specific root cause") || strings.Contains(strings.ToLower(text.String()), "quota") {
		t.Fatalf("invented scheduling diagnosis: %s", text.String())
	}
	encoded, err := json.Marshal(detailedStatus{PodGroups: groups})
	if err != nil || !bytes.Contains(encoded, []byte(`"podGroups":[{"name":"group"`)) {
		t.Fatalf("JSON=%s err=%v", encoded, err)
	}
}

func TestPodGroupStatusOptionalLookupDoesNotAffectOrdinaryPods(t *testing.T) {
	client := &podGroupDetailsFake{err: errors.New("forbidden")}
	if groups := gatherPodGroupStatus(context.Background(), "demo", []kube.PodSummary{{Name: "ordinary"}}, client); len(groups) != 0 || len(client.calls) != 0 {
		t.Fatalf("queried PodGroups for ordinary workload: %v", groups)
	}
	pods := []kube.PodSummary{{Name: "p", Annotations: map[string]string{"scheduling.volcano.sh/group-name": "group"}}}
	groups := gatherPodGroupStatus(context.Background(), "demo", pods, client)
	if len(groups) != 1 || groups[0].LookupError != "forbidden" || groups[0].Queue != "" {
		t.Fatalf("unavailable lookup presented as evidence: %+v", groups)
	}
	var text bytes.Buffer
	printPodGroupStatus(&text, groups)
	if !strings.Contains(text.String(), "details unavailable: forbidden") {
		t.Fatal(text.String())
	}
}
