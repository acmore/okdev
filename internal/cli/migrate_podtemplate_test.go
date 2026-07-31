package cli

import (
	"bytes"
	"reflect"
	"strings"
	"testing"
	"text/template"

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
	if got.Manifests[0].Path != "pod.yaml" {
		t.Fatalf("ManifestPath = %q, want pod.yaml", got.Manifests[0].Path)
	}

	// The manifest carries the container and its mounts, with the runtime
	// placeholder for the name so each run gets a fresh object.
	var pod corev1.Pod
	if err := yaml.Unmarshal(got.Manifests[0].Bytes, &pod); err != nil {
		t.Fatalf("unmarshal manifest: %v\n%s", err, got.Manifests[0].Bytes)
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
	if !strings.Contains(string(got.Manifests[0].Bytes), "{{ .WorkloadName }}") {
		t.Fatalf("manifest must keep the WorkloadName placeholder:\n%s", got.Manifests[0].Bytes)
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
	if got.Manifests[0].Path != ".okdev/pod.yaml" {
		t.Fatalf("ManifestPath = %q, want .okdev/pod.yaml", got.Manifests[0].Path)
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
	if len(got.Manifests) != 0 {
		t.Fatalf("no workload needs a manifest:\n%+v", got.Manifests)
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

// A podTemplate may legitimately contain Go-template braces — an arg for some
// other templating tool, say. Synthesized manifests used to be applied
// verbatim; manifest files are rendered as templates, so the migration has to
// escape what it extracts or `okdev up` fails on a config that worked before.
func TestPlanPodTemplateExtractionEscapesLiteralBraces(t *testing.T) {
	raw := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  podTemplate:
    spec:
      containers:
        - name: dev
          image: alpine
          args: ["--fmt={{ .Nope }}"]
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	manifest := string(got.Manifests[0].Bytes)
	// Rendering the way the runtime does must give back exactly what the user
	// wrote, with only the name substituted.
	tmpl, err := template.New("m").Option("missingkey=error").Parse(manifest)
	if err != nil {
		t.Fatalf("migrated manifest is not a valid template: %v\n%s", err, manifest)
	}
	var out bytes.Buffer
	if err := tmpl.Execute(&out, struct{ WorkloadName string }{"okdev-sess"}); err != nil {
		t.Fatalf("render migrated manifest: %v\n%s", err, manifest)
	}
	if !strings.Contains(out.String(), "--fmt={{ .Nope }}") {
		t.Fatalf("the user's braces must survive rendering:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "name: 'okdev-sess'") {
		t.Fatalf("the WorkloadName placeholder must still render:\n%s", out.String())
	}
}

// A config that never had a podTemplate ran on a container okdev injected when
// the pod spec declared none. That default was just another inline form, so the
// migration has to write it out as a real manifest — otherwise those configs
// have no way forward.
func TestPlanPodTemplateExtractionScaffoldsAManifestlessPodWorkload(t *testing.T) {
	raw := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/data"
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if !got.Applied {
		t.Fatal("a pod workload with no manifest must be given one")
	}
	if got.Extracted {
		t.Fatal("there was no podTemplate to extract")
	}
	manifest := string(got.Manifests[0].Bytes)
	if !strings.Contains(manifest, "image: ubuntu:22.04") {
		t.Fatalf("must reproduce the container okdev used to inject:\n%s", manifest)
	}
	// The mount has to follow the config's own sync remote, or the session comes
	// back up syncing into a directory nothing is mounted at.
	if !strings.Contains(manifest, "mountPath: /data") {
		t.Fatalf("the workspace mount must follow spec.sync:\n%s", manifest)
	}
	if len(got.Warnings) == 0 {
		t.Fatal("writing a manifest the user did not author must be reported")
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("migrated config must load: %v\n%s", err, got.ConfigBytes)
	}
	if cfg.Spec.Workloads[0].ManifestPath != "pod.yaml" {
		t.Fatalf("workload must point at the manifest: %+v", cfg.Spec.Workloads[0])
	}
}

// The migration must preserve meaning: the Pod okdev applies after migrating
// has to match what the inline config produced. This is the assertion the whole
// round exists to satisfy.
func TestMigratedConfigAppliesAnEquivalentPod(t *testing.T) {
	const runtimeName = "okdev-sess-1234"

	// What the inline config would have applied, built the old way.
	legacy, err := config.LoadPodTemplateOnly([]byte(inlinePodConfig))
	if err != nil {
		t.Fatal(err)
	}
	before, err := config.SynthesizePodManifest(legacy, runtimeName)
	if err != nil {
		t.Fatal(err)
	}

	// Migrate, then render the manifest the way the runtime now will.
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(inlinePodConfig))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	tmpl, err := template.New("m").Option("missingkey=error").Parse(string(got.Manifests[0].Bytes))
	if err != nil {
		t.Fatalf("migrated manifest is not a valid template: %v\n%s", err, got.Manifests[0].Bytes)
	}
	var rendered bytes.Buffer
	if err := tmpl.Execute(&rendered, struct{ WorkloadName string }{runtimeName}); err != nil {
		t.Fatalf("render migrated manifest: %v\n%s", err, got.Manifests[0].Bytes)
	}

	var beforePod, afterPod corev1.Pod
	if err := yaml.Unmarshal(before, &beforePod); err != nil {
		t.Fatal(err)
	}
	if err := yaml.Unmarshal(rendered.Bytes(), &afterPod); err != nil {
		t.Fatalf("unmarshal migrated manifest: %v\n%s", err, rendered.String())
	}
	if !reflect.DeepEqual(beforePod, afterPod) {
		t.Fatalf("migration changed the Pod:\n--- before ---\n%s\n--- after ---\n%s", before, rendered.String())
	}
}

// Several pod profiles could share one spec.podTemplate. Migrating only the
// first would report success and leave the config invalid, since manifestPath
// is now required on every profile.
func TestPlanPodTemplateExtractionGivesEveryPodProfileAManifest(t *testing.T) {
	raw := inlinePodConfig + `  workloads:
    - name: dev
      type: pod
    - name: big
      type: pod
    - name: train
      type: job
      manifestPath: train.yaml
`
	got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
	if err != nil {
		t.Fatalf("planPodTemplateExtraction: %v", err)
	}
	if len(got.Manifests) != 2 {
		t.Fatalf("both manifest-less pod profiles need a file, got %+v", got.Manifests)
	}
	// Named after the profile, so the two do not collide.
	if got.Manifests[0].Path != "dev.yaml" || got.Manifests[1].Path != "big.yaml" {
		t.Fatalf("manifests must be named after their profile: %+v", got.Manifests)
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("the migrated config must be valid, not just closer: %v\n%s", err, got.ConfigBytes)
	}
	if cfg.Spec.Workloads[2].ManifestPath != "train.yaml" {
		t.Fatalf("the job profile must be untouched: %+v", cfg.Spec.Workloads[2])
	}
	if len(got.Warnings) == 0 {
		t.Fatal("identical manifests for two profiles must be called out")
	}
}

// `podTemplate:` with nothing under it is a present key whose value decodes to
// a nil pointer. Deciding whether there was anything to extract from the key
// alone dereferenced that nil and panicked — in `okdev migrate` and, once init
// shared this resolution, in `okdev init` too.
func TestPlanPodTemplateExtractionSurvivesAnEmptyPodTemplateKey(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{name: "null", block: "  podTemplate:\n"},
		{name: "empty mapping", block: "  podTemplate: {}\n"},
		{name: "spec with no containers", block: "  podTemplate:\n    spec: {}\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
` + tc.block
			got, err := planPodTemplateExtraction("/repo/.okdev/okdev.yaml", []byte(raw))
			if err != nil {
				t.Fatalf("planPodTemplateExtraction: %v", err)
			}
			if !got.Applied {
				t.Fatal("a pod workload with no manifest still needs one")
			}
			if got.Extracted {
				t.Fatal("there was no container to extract")
			}
			if len(got.Manifests) != 1 {
				t.Fatalf("expected the starter manifest, got %+v", got.Manifests)
			}
			if !strings.Contains(string(got.Manifests[0].Bytes), "image: ubuntu:22.04") {
				t.Fatalf("expected the starter container:\n%s", got.Manifests[0].Bytes)
			}
			if _, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml"); err != nil {
				t.Fatalf("migrated config must load: %v\n%s", err, got.ConfigBytes)
			}
		})
	}
}
