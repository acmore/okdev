package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

const inlinePodConfig = `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  # keep me
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: ws
  podTemplate:
    metadata:
      labels:
        team: ml
    spec:
      containers:
        - name: dev
          image: nvidia/cuda:12.4.1-devel
          volumeMounts:
            - name: workspace
              mountPath: /workspace
`

func TestPlanPodTemplateExtraction(t *testing.T) {
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(inlinePodConfig))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if !got.Applied {
		t.Fatal("a config with podTemplate must be extracted")
	}
	if got.ManifestPath != "pod.yaml" {
		t.Fatalf("ManifestPath = %q, want pod.yaml", got.ManifestPath)
	}

	// The manifest carries the container and its mounts, with the runtime
	// placeholder for the name so each run gets a fresh object.
	var pod corev1.Pod
	if err := yaml.Unmarshal(got.ManifestBytes, &pod); err != nil {
		t.Fatalf("unmarshal manifest: %v\n%s", err, got.ManifestBytes)
	}
	if pod.Kind != "Pod" || pod.APIVersion != "v1" {
		t.Fatalf("type meta = %q/%q", pod.APIVersion, pod.Kind)
	}
	if pod.Labels["team"] != "ml" {
		t.Fatalf("podTemplate metadata.labels must survive: %v", pod.Labels)
	}
	if len(pod.Spec.Containers) != 1 || pod.Spec.Containers[0].Image != "nvidia/cuda:12.4.1-devel" {
		t.Fatalf("container did not survive: %+v", pod.Spec.Containers)
	}
	if len(pod.Spec.Containers[0].VolumeMounts) != 1 {
		t.Fatalf("volumeMounts travel with the container: %+v", pod.Spec.Containers[0].VolumeMounts)
	}
	if !strings.Contains(string(got.ManifestBytes), "{{ .WorkloadName }}") {
		t.Fatalf("manifest must keep the WorkloadName placeholder:\n%s", got.ManifestBytes)
	}

	// The config loses podTemplate, keeps volumes, keeps comments, and points
	// a workload at the manifest.
	cfgStr := string(got.ConfigBytes)
	if strings.Contains(cfgStr, "podTemplate") {
		t.Fatalf("podTemplate must be removed from the config:\n%s", cfgStr)
	}
	if !strings.Contains(cfgStr, "# keep me") {
		t.Fatalf("the edit stripped comments:\n%s", cfgStr)
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("migrated config must load: %v\n%s", err, cfgStr)
	}
	if len(cfg.Spec.Volumes) != 1 {
		t.Fatalf("spec.volumes stays in the config: %+v", cfg.Spec.Volumes)
	}
	names := cfg.WorkloadProfileNames()
	if len(names) != 1 || names[0] != config.DefaultWorkloadProfileName {
		t.Fatalf("profiles = %v, want [default]", names)
	}
	if cfg.Spec.Workloads[0].ManifestPath != "pod.yaml" {
		t.Fatalf("workload must point at the manifest: %+v", cfg.Spec.Workloads[0])
	}
}

func TestPlanPodTemplateExtractionNoop(t *testing.T) {
	raw := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  workloads:
    - name: default
      type: job
      manifestPath: job.yaml
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if got.Applied {
		t.Fatal("a config without podTemplate must not be touched")
	}
}

// A flat config keeps its location; its manifest still lands in .okdev/.
func TestPlanPodTemplateExtractionFlatConfig(t *testing.T) {
	got, err := planPodTemplateExtraction("/repo/.okdev.yaml", []byte(inlinePodConfig))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if got.ManifestPath != ".okdev/pod.yaml" {
		t.Fatalf("ManifestPath = %q, want .okdev/pod.yaml", got.ManifestPath)
	}
}

// The workload that was using podTemplate is the one with no manifestPath.
func TestPlanPodTemplateExtractionAttachesToTheManifestlessProfile(t *testing.T) {
	raw := inlinePodConfig + `  workloads:
    - name: dev
      type: pod
    - name: train
      type: job
      manifestPath: train.yaml
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("migrated config must load: %v\n%s", err, got.ConfigBytes)
	}
	if cfg.Spec.Workloads[0].Name != "dev" || cfg.Spec.Workloads[0].ManifestPath != "pod.yaml" {
		t.Fatalf("the manifest-less pod profile must receive it: %+v", cfg.Spec.Workloads[0])
	}
	if cfg.Spec.Workloads[1].ManifestPath != "train.yaml" {
		t.Fatalf("siblings must be untouched: %+v", cfg.Spec.Workloads[1])
	}
}

// A podTemplate no workload uses is dead config: drop it, but say so.
func TestPlanPodTemplateExtractionDropsAnUnusedPodTemplate(t *testing.T) {
	raw := inlinePodConfig + `  workloads:
    - name: train
      type: job
      manifestPath: train.yaml
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if len(got.ManifestBytes) != 0 {
		t.Fatalf("no workload needs a manifest:\n%s", got.ManifestBytes)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("dropping user config must warn")
	}
	if !strings.Contains(strings.Join(got.Warnings, " "), "podTemplate") {
		t.Fatalf("the warning must name what was dropped: %v", got.Warnings)
	}
	if strings.Contains(string(got.ConfigBytes), "podTemplate") {
		t.Fatalf("podTemplate must still be removed:\n%s", got.ConfigBytes)
	}
}

// The legacy singular spec.workload's settings must survive materialization —
// an attach.container the user set is otherwise silently lost.
func TestPlanPodTemplateExtractionCarriesLegacyWorkloadSettings(t *testing.T) {
	raw := inlinePodConfig + `  workload:
    type: pod
    attach:
      container: trainer
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("migrated config must load: %v\n%s", err, got.ConfigBytes)
	}
	if cfg.Spec.Workloads[0].Attach.Container != "trainer" {
		t.Fatalf("attach.container must survive: %+v\n%s", cfg.Spec.Workloads[0], got.ConfigBytes)
	}
}
