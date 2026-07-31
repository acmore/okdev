package cli

import (
	"context"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

type switchPodLister struct {
	pods []kube.PodSummary
}

func (s *switchPodLister) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return s.pods, nil
}

func TestDetectWorkloadSwitchOnDifferentProfile(t *testing.T) {
	k := &switchPodLister{pods: []kube.PodSummary{{
		Name:   "okdev-sess1-abcd",
		Labels: map[string]string{"okdev.io/workload-profile": "dev", "okdev.io/workload-type": "pod"},
	}}}
	live, liveType, isSwitch, err := detectWorkloadSwitch(context.Background(), k, "default", "sess1", "train", "pytorchjob")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if !isSwitch {
		t.Fatal("a different live profile must register as a switch")
	}
	if live != "dev" || liveType != "pod" {
		t.Fatalf("live = %q/%q, want dev/pod", live, liveType)
	}
}

func TestDetectWorkloadSwitchSameProfileIsNotASwitch(t *testing.T) {
	k := &switchPodLister{pods: []kube.PodSummary{{
		Name:   "okdev-sess1-abcd",
		Labels: map[string]string{"okdev.io/workload-profile": "train", "okdev.io/workload-type": "pytorchjob"},
	}}}
	_, _, isSwitch, err := detectWorkloadSwitch(context.Background(), k, "default", "sess1", "train", "pytorchjob")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if isSwitch {
		t.Fatal("the same profile must not register as a switch")
	}
}

func TestDetectWorkloadSwitchLegacyPodsFallBackToType(t *testing.T) {
	// Pods created before this feature carry no profile label. Type is then
	// the only signal, and a type change is still a switch.
	k := &switchPodLister{pods: []kube.PodSummary{{
		Name:   "okdev-sess1-abcd",
		Labels: map[string]string{"okdev.io/workload-type": "pod"},
	}}}
	_, _, isSwitch, err := detectWorkloadSwitch(context.Background(), k, "default", "sess1", "train", "job")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if !isSwitch {
		t.Fatal("an unlabelled pod with a different type must register as a switch")
	}

	k2 := &switchPodLister{pods: []kube.PodSummary{{
		Name:   "okdev-sess1-abcd",
		Labels: map[string]string{"okdev.io/workload-type": "pod"},
	}}}
	_, _, isSwitch, err = detectWorkloadSwitch(context.Background(), k2, "default", "sess1", "default", "pod")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if isSwitch {
		t.Fatal("an unlabelled pod with a matching type must not register as a switch")
	}
}

func TestDetectWorkloadSwitchIgnoresTerminatingPods(t *testing.T) {
	// A pod on its way out is not the workload the session is running; acting
	// on it would prompt for a delete of something already deleted.
	k := &switchPodLister{pods: []kube.PodSummary{{
		Name:     "okdev-sess1-old",
		Deleting: true,
		Labels:   map[string]string{"okdev.io/workload-profile": "dev", "okdev.io/workload-type": "pod"},
	}}}
	_, _, isSwitch, err := detectWorkloadSwitch(context.Background(), k, "default", "sess1", "train", "job")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if isSwitch {
		t.Fatal("a terminating pod must not register as a switch")
	}
}

func TestDetectWorkloadSwitchNoPods(t *testing.T) {
	k := &switchPodLister{}
	_, _, isSwitch, err := detectWorkloadSwitch(context.Background(), k, "default", "sess1", "train", "job")
	if err != nil {
		t.Fatalf("detectWorkloadSwitch: %v", err)
	}
	if isSwitch {
		t.Fatal("no live pods means nothing to switch away from")
	}
}
