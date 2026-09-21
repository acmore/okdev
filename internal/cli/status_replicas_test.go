package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
)

func TestReplicaStatusSeparatesRolesAndLifecycle(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	dir := t.TempDir()
	path := filepath.Join(dir, "workload.json")
	err := os.WriteFile(path, []byte(`{"apiVersion":"kubeflow.org/v1","kind":"PyTorchJob","metadata":{"name":"train"},"spec":{"pytorchReplicaSpecs":{"Master":{"replicas":1},"Worker":{"replicas":4}}}}`), 0600)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = path
	pod := func(role, phase, ready string) kube.PodSummary {
		return kube.PodSummary{Phase: phase, Ready: ready, Labels: map[string]string{"okdev.io/workload-name": "train", "okdev.io/workload-resource-kind": "PyTorchJob", "training.kubeflow.org/replica-type": role}}
	}
	deleting := pod("worker", "Running", "2/2")
	deleting.Deleting = true
	foreign := pod("worker", "Running", "2/2")
	foreign.Labels["okdev.io/workload-name"] = "another"
	view := sessionView{Session: "train", Pods: []kube.PodSummary{pod("master", "Running", "2/2"), pod("worker", "Running", "1/2"), pod("worker", "Pending", "0/2"), pod("worker", "Succeeded", "0/2"), pod("worker", "Failed", "0/2"), deleting, foreign}}
	got := buildReplicaStatus(cfg, filepath.Join(dir, "okdev.yaml"), view)
	if got == nil || got.Unavailable != "" || len(got.Groups) != 2 {
		t.Fatalf("%+v", got)
	}
	master, worker := got.Groups[0], got.Groups[1]
	if master.CountMismatch || master.Present != 1 || master.Ready != 1 {
		t.Fatalf("master: %+v", master)
	}
	if !worker.CountMismatch || worker.Present != 2 || worker.Running != 1 || worker.Ready != 0 {
		t.Fatalf("worker: %+v", worker)
	}
	var out bytes.Buffer
	printReplicaStatus(&out, got)
	if !strings.Contains(out.String(), "Worker: manifest declares 4; 2 present, 1 running, 0 ready; count differs") {
		t.Fatal(out.String())
	}
	view.Pods = nil
	got = buildReplicaStatus(cfg, filepath.Join(dir, "okdev.yaml"), view)
	if got.Groups[1].Present != 0 || !got.Groups[1].CountMismatch {
		t.Fatalf("zero pods: %+v", got)
	}
	os.Remove(path)
	got = buildReplicaStatus(cfg, filepath.Join(dir, "okdev.yaml"), view)
	if got == nil || got.Unavailable == "" {
		t.Fatal("missing manifest must be unknown, not zero")
	}
}
