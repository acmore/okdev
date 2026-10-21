package cli

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func TestDetectPodReplacementExplainsControllerReplacedPod(t *testing.T) {
	// #325: a Deployment replaced its Running pod within the same run; the
	// replacement sits Pending and nothing else says the old pod existed.
	snapshot := session.LastSeen{
		At:        time.Now().Add(-10 * time.Minute),
		RunID:     "run1",
		Namespace: "default",
		Pods:      []session.LastSeenPod{{Name: "okdev-s-run1-rs-old", UID: "u-old", Phase: "Running", Node: "gpu-node-3"}},
	}
	events := &fakeEventLister{events: map[string][]kube.EventSummary{
		"okdev-s-run1-rs-old": {{Reason: "Preempted", Message: "Preempted by a higher-priority pod", InvolvedName: "okdev-s-run1-rs-old", InvolvedUID: "u-old"}},
	}}
	live := []kube.PodSummary{{Name: "okdev-s-run1-rs-new", UID: "u-new", Phase: "Pending"}}

	record, ok := detectPodReplacement(context.Background(), events, "default", snapshot, "run1", live)
	if !ok {
		t.Fatal("expected the replaced pod to be reported")
	}
	if record.Class != session.RunEndClassEvicted || len(record.ReplacedBy) != 1 || record.ReplacedBy[0] != "okdev-s-run1-rs-new" {
		t.Fatalf("unexpected record: %+v", record)
	}
	line := formatPodReplacement(record)
	for _, want := range []string{"pod okdev-s-run1-rs-old replaced by okdev-s-run1-rs-new", "evicted or preempted", "Preempted", "on node gpu-node-3"} {
		if !strings.Contains(line, want) {
			t.Fatalf("line %q missing %q", line, want)
		}
	}
}

func TestDetectPodReplacementMatchesSameNameRecreationByUID(t *testing.T) {
	// PyTorchJob recreates pods under the same name: the name check alone is
	// blind, and the new pod's events share the name.
	snapshot := session.LastSeen{
		At:    time.Now(),
		RunID: "run1",
		Pods:  []session.LastSeenPod{{Name: "job-worker-0", UID: "u-old", Phase: "Running"}},
	}
	events := &fakeEventLister{events: map[string][]kube.EventSummary{
		"job-worker-0": {
			{Reason: "Evicted", Message: "The node was low on resource: memory", InvolvedUID: "u-old"},
			{Reason: "Killing", Message: "Stopping container", InvolvedUID: "u-new"},
		},
	}}
	live := []kube.PodSummary{{Name: "job-worker-0", UID: "u-new", Phase: "Pending"}}
	record, ok := detectPodReplacement(context.Background(), events, "default", snapshot, "run1", live)
	if !ok || record.Class != session.RunEndClassEvicted || !strings.Contains(record.Evidence, "Evicted") {
		t.Fatalf("expected the old pod's eviction, got ok=%v %+v", ok, record)
	}

	// The same pod still live is not a replacement.
	live[0].UID = "u-old"
	if _, ok := detectPodReplacement(context.Background(), events, "default", snapshot, "run1", live); ok {
		t.Fatal("unchanged pod reported as replaced")
	}
}

func TestDetectPodReplacementStaysSilentOutsideTheRun(t *testing.T) {
	live := []kube.PodSummary{{Name: "new", UID: "u-new"}}
	for name, snapshot := range map[string]session.LastSeen{
		"other run":     {At: time.Now(), RunID: "run0", Pods: []session.LastSeenPod{{Name: "old"}}},
		"no run id":     {At: time.Now(), Pods: []session.LastSeenPod{{Name: "old"}}},
		"no snapshot":   {},
		"pod completed": {At: time.Now(), RunID: "run1", Pods: []session.LastSeenPod{{Name: "old", Phase: "Succeeded"}}},
	} {
		if record, ok := detectPodReplacement(context.Background(), nil, "default", snapshot, "run1", live); ok {
			t.Fatalf("%s: unexpected report %+v", name, record)
		}
	}
	snapshot := session.LastSeen{At: time.Now(), RunID: "run1", Pods: []session.LastSeenPod{{Name: "old"}}}
	if _, ok := detectPodReplacement(context.Background(), nil, "default", snapshot, "run1", nil); ok {
		t.Fatal("zero live pods belongs to the session-death report")
	}
}

func TestObserveSessionPodsRecordsBeforeRefreshingSnapshot(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	info := session.Info{Name: "sess1", Namespace: "default", RunID: "run1"}
	if err := session.SaveInfo(info); err != nil {
		t.Fatal(err)
	}
	oldSnapshot := session.LastSeen{
		At: time.Now().Add(-time.Minute), RunID: "run1", Namespace: "default",
		Pods: []session.LastSeenPod{{Name: "old", UID: "u-old", Phase: "Running", Node: "n1"}},
	}
	if err := session.SaveLastSeen("sess1", oldSnapshot); err != nil {
		t.Fatal(err)
	}
	events := &fakeEventLister{events: map[string][]kube.EventSummary{
		"old": {{Reason: "Evicted", Message: "node pressure", InvolvedUID: "u-old"}},
	}}
	live := []kube.PodSummary{{Name: "new", UID: "u-new", Phase: "Pending"}}

	record, newly := observeSessionPods(context.Background(), events, info, "default", live, "")
	if !newly || record.Class != session.RunEndClassEvicted {
		t.Fatalf("expected a new eviction record, got newly=%v %+v", newly, record)
	}
	snapshot, err := session.LoadLastSeen("sess1")
	if err != nil || len(snapshot.Pods) != 1 || snapshot.Pods[0].Name != "new" || snapshot.Pods[0].UID != "u-new" {
		t.Fatalf("snapshot not refreshed to the live pod: %+v (%v)", snapshot, err)
	}

	// The record outlives the snapshot refresh and is shown for this run.
	if _, newly := observeSessionPods(context.Background(), events, info, "default", live, ""); newly {
		t.Fatal("a repeat observation must not re-report")
	}
	stored, ok := currentSessionPodReplacement("sess1")
	if !ok || stored.Ended[0].Name != "old" {
		t.Fatalf("expected the stored record, got ok=%v %+v", ok, stored)
	}

	// Seeing the same ended pod again after events expired must not
	// replace the evidence with "unknown".
	if err := session.SaveLastSeen("sess1", oldSnapshot); err != nil {
		t.Fatal(err)
	}
	if _, newly := observeSessionPods(context.Background(), &fakeEventLister{}, info, "default", live, ""); newly {
		t.Fatal("same ended pods must keep the original record")
	}
	if stored, _ := currentSessionPodReplacement("sess1"); stored.Class != session.RunEndClassEvicted {
		t.Fatalf("evidence overwritten: %+v", stored)
	}

	// A different run hides it.
	info.RunID = "run2"
	if err := session.SaveInfo(info); err != nil {
		t.Fatal(err)
	}
	if _, ok := currentSessionPodReplacement("sess1"); ok {
		t.Fatal("record must be scoped to the run it describes")
	}
}

func TestObserveSessionPodsRetainsMultipleReplacements(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	info := session.Info{Name: "sess-history", Namespace: "default", RunID: "run1"}
	if err := session.SaveInfo(info); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveLastSeen(info.Name, session.LastSeen{
		At: time.Now(), RunID: info.RunID, Namespace: info.Namespace,
		Pods: []session.LastSeenPod{{Name: "first", UID: "u1", Phase: "Running"}},
	}); err != nil {
		t.Fatal(err)
	}
	for _, pod := range []kube.PodSummary{
		{Name: "second", UID: "u2", Phase: "Running"},
		{Name: "third", UID: "u3", Phase: "Running"},
	} {
		if _, newly := observeSessionPods(context.Background(), nil, info, info.Namespace, []kube.PodSummary{pod}, ""); !newly {
			t.Fatalf("replacement by %s not recorded", pod.Name)
		}
	}
	record, ok := currentSessionPodReplacement(info.Name)
	if !ok || record.Ended[0].Name != "second" || len(record.History) != 1 || record.History[0].Ended[0].Name != "first" {
		t.Fatalf("earlier replacement lost: %+v", record)
	}
	var out strings.Builder
	printPodReplacement(&out, record)
	if !strings.Contains(out.String(), "pod first replaced by second") || !strings.Contains(out.String(), "pod second replaced by third") {
		t.Fatalf("status omitted replacement history: %s", out.String())
	}
}

func TestDetectPodReplacementIgnoresRolloutRetiredPods(t *testing.T) {
	// `up --reconcile` rolls a Deployment: the old ReplicaSet's pod goes
	// away by design and must not be reported as an unexplained end.
	snapshot := session.LastSeen{
		At: time.Now(), RunID: "run1",
		Pods: []session.LastSeenPod{
			{Name: "dep-aaa-1", UID: "u1", Revision: "aaa", Phase: "Running"},
			{Name: "dep-bbb-1", UID: "u2", Revision: "bbb", Phase: "Running"},
		},
	}
	now := time.Now()
	live := []kube.PodSummary{
		{Name: "dep-bbb-1", UID: "u2", CreatedAt: now.Add(-time.Minute), Labels: map[string]string{"pod-template-hash": "bbb"}},
		{Name: "dep-bbb-2", UID: "u3", CreatedAt: now, Labels: map[string]string{"pod-template-hash": "bbb"}},
	}
	if record, ok := detectPodReplacement(context.Background(), nil, "default", snapshot, "run1", live); ok {
		t.Fatalf("rollout-retired pod reported: %+v", record)
	}
	// A pod of the current revision that vanished is still reported.
	snapshot.Pods = append(snapshot.Pods, session.LastSeenPod{Name: "dep-bbb-0", UID: "u0", Revision: "bbb", Phase: "Running"})
	record, ok := detectPodReplacement(context.Background(), nil, "default", snapshot, "run1", live)
	if !ok || len(record.Ended) != 1 || record.Ended[0].Name != "dep-bbb-0" {
		t.Fatalf("expected only the current-revision pod, got ok=%v %+v", ok, record)
	}
}
