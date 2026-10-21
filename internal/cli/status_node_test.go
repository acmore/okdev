package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	corev1 "k8s.io/api/core/v1"
)

func TestStatusNodeChangeSurvivesExecRefreshingTheSnapshot(t *testing.T) {
	// #344: the pod moves, and exec (which refreshes the snapshot through
	// observeSessionPods) runs before anyone looks at status. The move must
	// still be reported for the rest of the run.
	t.Setenv("HOME", t.TempDir())
	info := session.Info{Name: "demo", Namespace: "default", KubeContext: "dev", RunID: "run-a"}
	if err := session.SaveInfo(info); err != nil {
		t.Fatal(err)
	}
	if err := session.SaveLastSeen("demo", session.LastSeen{
		At: time.Now(), Namespace: "default", Context: "dev", RunID: "run-a",
		Pods: []session.LastSeenPod{{Name: "old-pod", Node: "node-a"}},
	}); err != nil {
		t.Fatal(err)
	}
	moved := []kube.PodSummary{{Name: "new-pod", NodeName: "node-b"}}
	observeSessionPods(context.Background(), nil, info, "default", moved, "dev") // exec
	view := sessionView{Namespace: "default", Session: "demo", TargetPod: "new-pod", Pods: moved}
	for i := 0; i < 2; i++ { // status, twice: the record must not wear off
		captureSessionLastSeen(context.Background(), nil, []sessionView{view}, "dev")
		changes := statusNodeChanges(view, "dev")
		if changes["new-pod"] != "node-a" || targetPodNode(view) != "node-b" {
			t.Fatalf("status %d: node move lost: %+v", i, changes)
		}
		var out bytes.Buffer
		printStatusNodeChanges(&out, view, changes)
		if !strings.Contains(out.String(), "moved from node node-a to node-b") {
			t.Fatalf("missing move warning: %q", out.String())
		}
	}
	if got := statusNodeChanges(view, "another-context"); len(got) != 0 {
		t.Fatalf("cross-context node warning: %+v", got)
	}
	// A new run starts clean.
	info.RunID = "run-b"
	if err := session.SaveInfo(info); err != nil {
		t.Fatal(err)
	}
	if got := statusNodeChanges(view, "dev"); len(got) != 0 {
		t.Fatalf("node move leaked into the next run: %+v", got)
	}
}

func TestNodeChangesSince(t *testing.T) {
	previous := session.LastSeen{
		Pods:        []session.LastSeenPod{{Name: "a", Node: "n1"}, {Name: "b", Node: "n2"}},
		NodeChanges: map[string]string{"b": "n0", "gone": "n9"},
	}
	got := nodeChangesSince(previous, []kube.PodSummary{{Name: "a", NodeName: "n3"}, {Name: "b", NodeName: "n2"}})
	if len(got) != 2 || got["a"] != "n1" || got["b"] != "n0" {
		t.Fatalf("expected a new move and a retained one, got %+v", got)
	}
	if got := nodeChangesSince(previous, []kube.PodSummary{{Name: "a", NodeName: "n1"}}); got != nil {
		t.Fatalf("no move and no retained live pod should be nil, got %+v", got)
	}
}

func TestUpStorageNoticeClassifiesMountedVolumes(t *testing.T) {
	pod := &corev1.Pod{Spec: corev1.PodSpec{
		Volumes: []corev1.Volume{
			{Name: "scratch", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			{Name: "host", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/scratch"}}},
			{Name: "results", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "results"}}},
		},
		Containers: []corev1.Container{{Name: "dev", VolumeMounts: []corev1.VolumeMount{{Name: "scratch", MountPath: "/tmp/scratch"}, {Name: "host", MountPath: "/data"}, {Name: "results", MountPath: "/results"}}}},
	}}
	var out bytes.Buffer
	printUpStorageNotice(&out, pod, "dev")
	for _, want := range []string{"dev:/tmp/scratch — node-local emptyDir", "dev:/data — node-local hostPath", "dev:/results — PVC-backed"} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("missing %q in %q", want, out.String())
		}
	}
}
