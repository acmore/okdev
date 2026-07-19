package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestNewRestartCmdFlags(t *testing.T) {
	cmd := newRestartCmd(&Options{})
	if cmd.Use != "restart [session]" {
		t.Fatalf("unexpected use: %q", cmd.Use)
	}
	yes := cmd.Flags().Lookup("yes")
	if yes == nil || yes.Shorthand != "y" {
		t.Fatalf("expected --yes/-y flag, got %+v", yes)
	}
	wt := cmd.Flags().Lookup("wait-timeout")
	if wt == nil || wt.DefValue != upDefaultWaitTimeout.String() {
		t.Fatalf("expected --wait-timeout default %s, got %+v", upDefaultWaitTimeout, wt)
	}
}

func TestConfirmRestartRefusesNonInteractive(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmRestart(strings.NewReader("y\n"), &out, "sess", "default", "Pod", "okdev-sess")
	if err == nil || ok {
		t.Fatalf("expected non-interactive refusal, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error should point at --yes, got %v", err)
	}
}

func TestNewRestartCmdPodFlag(t *testing.T) {
	cmd := newRestartCmd(&Options{})
	pod := cmd.Flags().Lookup("pod")
	if pod == nil || pod.Value.Type() != "stringSlice" {
		t.Fatalf("expected --pod string slice flag, got %+v", pod)
	}
}

type fakeRestartPolicyClient struct {
	policies map[string]string // role -> policy
	missing  bool
}

func (f *fakeRestartPolicyClient) GetResourceNestedString(_ context.Context, _, _, _, _ string, fields ...string) (string, bool, error) {
	if f.missing {
		return "", false, nil
	}
	role := fields[len(fields)-2]
	policy, ok := f.policies[role]
	return policy, ok, nil
}

func ptjobPod(name, role string) kube.PodSummary {
	return kube.PodSummary{
		Name: name,
		Labels: map[string]string{
			"okdev.io/workload-type": "pytorchjob",
			"okdev.io/workload-name": "okdev-sess-abc",
			"okdev.io/workload-role": role,
		},
	}
}

func TestEnsureControllerRecreatesDeletedPodsRefusesNever(t *testing.T) {
	k := &fakeRestartPolicyClient{policies: map[string]string{"Worker": "Never"}}
	err := ensureControllerRecreatesDeletedPods(context.Background(), k, "default", "sess", []kube.PodSummary{ptjobPod("okdev-sess-abc-worker-1", "Worker")})
	if err == nil {
		t.Fatal("expected refusal for restartPolicy Never")
	}
	for _, want := range []string{"restartPolicy", "OnFailure", "--reconcile"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error should mention %q, got: %v", want, err)
		}
	}
}

func TestEnsureControllerRecreatesDeletedPodsRefusesUnsetPolicy(t *testing.T) {
	k := &fakeRestartPolicyClient{missing: true}
	err := ensureControllerRecreatesDeletedPods(context.Background(), k, "default", "sess", []kube.PodSummary{ptjobPod("okdev-sess-abc-worker-1", "Worker")})
	if err == nil || !strings.Contains(err.Error(), "Never (default)") {
		t.Fatalf("expected refusal for unset restartPolicy, got: %v", err)
	}
}

func TestEnsureControllerRecreatesDeletedPodsAllowsOnFailure(t *testing.T) {
	k := &fakeRestartPolicyClient{policies: map[string]string{"Worker": "OnFailure", "Master": "OnFailure"}}
	err := ensureControllerRecreatesDeletedPods(context.Background(), k, "default", "sess", []kube.PodSummary{
		ptjobPod("okdev-sess-abc-worker-1", "Worker"),
		ptjobPod("okdev-sess-abc-master-0", "Master"),
	})
	if err != nil {
		t.Fatalf("expected OnFailure to pass, got: %v", err)
	}
}

func TestEnsureControllerRecreatesDeletedPodsIgnoresOtherWorkloads(t *testing.T) {
	k := &fakeRestartPolicyClient{missing: true}
	pod := kube.PodSummary{Name: "dep-abc123", Labels: map[string]string{"okdev.io/workload-type": "generic"}}
	if err := ensureControllerRecreatesDeletedPods(context.Background(), k, "default", "sess", []kube.PodSummary{pod}); err != nil {
		t.Fatalf("non-pytorchjob pods should not be gated, got: %v", err)
	}
}

func TestCountPodReplacementsStableNames(t *testing.T) {
	preDelete := []kube.PodSummary{
		{Name: "w-0", UID: "uid-0", Phase: "Running"},
		{Name: "w-1", UID: "uid-1", Phase: "Running"},
	}
	deleted := []kube.PodSummary{{Name: "w-1", UID: "uid-1"}}

	// Old pod still terminating: not replaced yet.
	replaced, pending := countPodReplacements([]kube.PodSummary{
		{Name: "w-0", UID: "uid-0", Phase: "Running"},
		{Name: "w-1", UID: "uid-1", Phase: "Terminating", Deleting: true},
	}, preDelete, deleted)
	if replaced != 0 || len(pending) != 1 || pending[0] != "w-1" {
		t.Fatalf("terminating old pod should be pending, got replaced=%d pending=%v", replaced, pending)
	}

	// Same name, new UID, Running: replaced.
	replaced, pending = countPodReplacements([]kube.PodSummary{
		{Name: "w-0", UID: "uid-0", Phase: "Running"},
		{Name: "w-1", UID: "uid-1b", Phase: "Running"},
	}, preDelete, deleted)
	if replaced != 1 || len(pending) != 0 {
		t.Fatalf("recreated pod should count and clear pending, got replaced=%d pending=%v", replaced, pending)
	}
}

func TestCountPodReplacementsFreshNames(t *testing.T) {
	preDelete := []kube.PodSummary{
		{Name: "dep-aaa", UID: "uid-a", Phase: "Running"},
		{Name: "dep-bbb", UID: "uid-b", Phase: "Running"},
	}
	deleted := []kube.PodSummary{{Name: "dep-bbb", UID: "uid-b"}}

	replaced, _ := countPodReplacements([]kube.PodSummary{
		{Name: "dep-aaa", UID: "uid-a", Phase: "Running"},
		{Name: "dep-ccc", UID: "uid-c", Phase: "Running"},
	}, preDelete, deleted)
	if replaced != 1 {
		t.Fatalf("fresh-name successor should count, got replaced=%d", replaced)
	}

	// A pod that existed before the delete does not count as a successor.
	replaced, _ = countPodReplacements([]kube.PodSummary{
		{Name: "dep-aaa", UID: "uid-a", Phase: "Running"},
	}, preDelete, deleted)
	if replaced != 0 {
		t.Fatalf("pre-existing pod must not count, got replaced=%d", replaced)
	}
}

type fakeReplacementLister struct {
	responses [][]kube.PodSummary
	calls     int
}

func (f *fakeReplacementLister) ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error) {
	i := f.calls
	if i >= len(f.responses) {
		i = len(f.responses) - 1
	}
	f.calls++
	return f.responses[i], nil
}

func TestWaitForPodReplacementsSucceedsOnRecreation(t *testing.T) {
	preDelete := []kube.PodSummary{{Name: "w-1", UID: "uid-1", Phase: "Running"}}
	lister := &fakeReplacementLister{responses: [][]kube.PodSummary{
		{{Name: "w-1", UID: "uid-1b", Phase: "Running"}},
	}}
	err := waitForPodReplacements(context.Background(), lister, "default", "sess", preDelete, preDelete, 5*time.Second, nil)
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
}

func TestWaitForPodReplacementsTimesOut(t *testing.T) {
	preDelete := []kube.PodSummary{{Name: "w-1", UID: "uid-1", Phase: "Running"}}
	lister := &fakeReplacementLister{responses: [][]kube.PodSummary{{}}}
	err := waitForPodReplacements(context.Background(), lister, "default", "sess", preDelete, preDelete, 100*time.Millisecond, nil)
	if err == nil || !strings.Contains(err.Error(), "did not appear") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}

func TestConfirmRestartPodsRefusesNonInteractive(t *testing.T) {
	var out bytes.Buffer
	ok, err := confirmRestartPods(strings.NewReader("y\n"), &out, "sess", "default", []string{"w-1"})
	if err == nil || ok {
		t.Fatalf("expected non-interactive refusal, got ok=%v err=%v", ok, err)
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("error should point at --yes, got %v", err)
	}
}
