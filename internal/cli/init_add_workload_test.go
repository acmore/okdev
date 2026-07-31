package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestValidateInitInvocation(t *testing.T) {
	cases := []struct {
		name    string
		inv     initInvocation
		wantErr string // substring; "" means it must be accepted
	}{
		{
			name: "fresh config, plain init",
			inv:  initInvocation{},
		},
		{
			name: "fresh config with project flags",
			inv:  initInvocation{ProjectFlagsSet: []string{"namespace"}},
		},
		{
			name:    "fresh config with a workload name",
			inv:     initInvocation{WorkloadName: "train"},
			wantErr: "--workload-name",
		},
		{
			name: "existing config, additive",
			inv:  initInvocation{ConfigExists: true, WorkloadName: "train"},
		},
		{
			name:    "existing config without a workload name",
			inv:     initInvocation{ConfigExists: true},
			wantErr: "already exists",
		},
		{
			name: "existing config with --force rewrites wholesale",
			inv:  initInvocation{ConfigExists: true, Force: true},
		},
		{
			name:    "existing config with --force and a workload name",
			inv:     initInvocation{ConfigExists: true, Force: true, WorkloadName: "train"},
			wantErr: "--force",
		},
		{
			name:    "existing config with a project flag",
			inv:     initInvocation{ConfigExists: true, WorkloadName: "train", ProjectFlagsSet: []string{"namespace", "ssh-user"}},
			wantErr: "namespace",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateInitInvocation(tc.inv)
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("expected the invocation to be accepted, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected an error mentioning %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q should mention %q", err, tc.wantErr)
			}
		})
	}
}

func TestInitAdditiveMode(t *testing.T) {
	if !initAdditiveMode(initInvocation{ConfigExists: true, WorkloadName: "train"}) {
		t.Error("an existing config plus a workload name is the additive path")
	}
	if initAdditiveMode(initInvocation{WorkloadName: "train"}) {
		t.Error("a fresh config is never additive")
	}
	if initAdditiveMode(initInvocation{ConfigExists: true, Force: true}) {
		t.Error("--force rewrites wholesale, it does not append")
	}
}

func TestAdditiveManifestPathKeepsManifestsInsideDotOkdev(t *testing.T) {
	// Folder config: ManifestDir is already .okdev/, so a bare name lands there.
	if got := additiveManifestPath("/repo/.okdev/okdev.yaml", "train"); got != "train.yaml" {
		t.Fatalf("folder config path = %q, want train.yaml", got)
	}
	// Flat config: ManifestDir is the project root, so the path has to say
	// .okdev/ explicitly or the manifest lands in the repo root.
	if got := additiveManifestPath("/repo/.okdev.yaml", "train"); got != ".okdev/train.yaml" {
		t.Fatalf("flat config path = %q, want .okdev/train.yaml", got)
	}
}

func planFixture(t *testing.T, raw string, workloadType, name string, inject []string) (*workloadAddition, error) {
	t.Helper()
	cfg, _, err := config.LoadFromBytes([]byte(raw), "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	vars := config.NewTemplateVars()
	vars.WorkloadType = workloadType
	vars.InjectPaths = inject
	return planWorkloadAddition("/repo/.okdev/okdev.yaml", []byte(raw), cfg, vars, name)
}

const podOnlyConfig = `apiVersion: okdev.io/v1alpha1
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
`

func TestPlanWorkloadAdditionScaffoldsEveryType(t *testing.T) {
	cases := []struct {
		workloadType string
		inject       []string
	}{
		{workloadType: "job"},
		{workloadType: "pytorchjob"},
		{workloadType: "pod"},
	}
	for _, tc := range cases {
		t.Run(tc.workloadType, func(t *testing.T) {
			add, err := planFixture(t, podOnlyConfig, tc.workloadType, "extra", tc.inject)
			if err != nil {
				t.Fatalf("planWorkloadAddition: %v", err)
			}
			if len(add.ManifestBytes) == 0 {
				t.Fatal("expected a scaffolded manifest")
			}
			// The planned config must be valid — this is the whole point.
			cfg, _, err := config.LoadFromBytes(add.ConfigBytes, "/repo/.okdev/okdev.yaml")
			if err != nil {
				t.Fatalf("planned config does not load: %v\n%s", err, add.ConfigBytes)
			}
			names := cfg.WorkloadProfileNames()
			if len(names) != 2 || names[0] != config.DefaultWorkloadProfileName || names[1] != "extra" {
				t.Fatalf("profiles = %v, want [default extra]\n%s", names, add.ConfigBytes)
			}
		})
	}
}

func TestPlanWorkloadAdditionCopiesPodTemplateForPod(t *testing.T) {
	add, err := planFixture(t, podOnlyConfig, "pod", "big", nil)
	if err != nil {
		t.Fatalf("planWorkloadAddition: %v", err)
	}
	manifest := string(add.ManifestBytes)
	if !strings.Contains(manifest, "alpine") {
		t.Fatalf("a pod workload must start as a copy of spec.podTemplate:\n%s", manifest)
	}
	// The name must stay a runtime placeholder so each run gets a fresh object
	// name, exactly like the job/pytorchjob scaffolds.
	if !strings.Contains(manifest, "{{ .WorkloadName }}") {
		t.Fatalf("scaffolded pod manifest must keep the WorkloadName placeholder:\n%s", manifest)
	}
}

func TestPlanWorkloadAdditionRejectsGenericWithoutInject(t *testing.T) {
	_, err := planFixture(t, podOnlyConfig, "generic", "gen", nil)
	if err == nil {
		t.Fatal("generic without --inject-path must be rejected before anything is written")
	}
}

func TestPlanWorkloadAdditionRejectsDuplicateName(t *testing.T) {
	raw := podOnlyConfig + `  workloads:
    - name: default
      type: pod
    - name: taken
      type: job
      manifestPath: taken.yaml
`
	if _, err := planFixture(t, raw, "job", "taken", nil); err == nil {
		t.Fatal("a duplicate workload name must be rejected")
	}
}

func TestPlanWorkloadAdditionPreservesComments(t *testing.T) {
	raw := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  # keep me
  namespace: default
  podTemplate:
    spec:
      containers:
        - name: dev
          image: alpine
`
	add, err := planFixture(t, raw, "job", "batch", nil)
	if err != nil {
		t.Fatalf("planWorkloadAddition: %v", err)
	}
	if !strings.Contains(string(add.ConfigBytes), "# keep me") {
		t.Fatalf("the edit stripped comments:\n%s", add.ConfigBytes)
	}
}

func TestAppendWorkloadProfilePreservesCommentsAndOrder(t *testing.T) {
	original := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  # keep me: this comment must survive the edit
  namespace: default
  workloads:
    - name: dev
      type: pod
`
	raw, err := appendWorkloadProfileToConfigBytes([]byte(original), config.WorkloadProfile{
		Name: "train", Type: "job", ManifestPath: "job.yaml",
	})
	if err != nil {
		t.Fatalf("appendWorkloadProfileToConfigBytes: %v", err)
	}
	got := string(raw)
	if !strings.Contains(got, "# keep me") {
		t.Fatalf("the edit stripped comments:\n%s", got)
	}
	if !strings.Contains(got, "name: train") || !strings.Contains(got, "manifestPath: job.yaml") {
		t.Fatalf("the new profile was not appended:\n%s", got)
	}
	if !strings.Contains(got, "name: dev") {
		t.Fatalf("the edit dropped the existing profile:\n%s", got)
	}
}

func TestAppendWorkloadProfileRejectsDuplicateName(t *testing.T) {
	original := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  workloads:
    - name: dev
      type: pod
`
	_, err := appendWorkloadProfileToConfigBytes([]byte(original), config.WorkloadProfile{Name: "dev", Type: "job", ManifestPath: "j.yaml"})
	if err == nil || !strings.Contains(err.Error(), "dev") {
		t.Fatalf("expected a duplicate-name error, got %v", err)
	}
}

func TestAppendWorkloadProfileMaterializesLegacySingularWorkload(t *testing.T) {
	// A config that only has spec.workload must not lose the workload it is
	// already running when a second one is declared.
	original := []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  workload:
    type: job
    manifestPath: job.yaml
    inject:
      - path: spec.template
`)
	raw, err := appendWorkloadProfileToConfigBytes(original, config.WorkloadProfile{
		Name: "train", Type: "pytorchjob", ManifestPath: "pt.yaml",
		Inject: []config.WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Worker.template"}},
	})
	if err != nil {
		t.Fatalf("appendWorkloadProfileToConfigBytes: %v", err)
	}
	cfg, _, err := config.LoadFromBytes(raw, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("the rewritten config must still load: %v\n%s", err, raw)
	}
	names := cfg.WorkloadProfileNames()
	if len(names) != 2 || names[0] != config.DefaultWorkloadProfileName || names[1] != "train" {
		t.Fatalf("profiles = %v, want [default train]\n%s", names, raw)
	}
	if err := cfg.SelectWorkload(config.DefaultWorkloadProfileName); err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.Workload.Type != "job" || cfg.Spec.Workload.ManifestPath != "job.yaml" {
		t.Fatalf("the legacy workload was not preserved: %+v\n%s", cfg.Spec.Workload, raw)
	}
}

func TestAppendWorkloadProfileMaterializesImplicitPodWorkload(t *testing.T) {
	// `okdev init` omits spec.workload entirely for pod configs — pod is the
	// default. Declaring a second workload must still leave the pod one
	// declared, or the workload the session is running silently disappears.
	original := []byte(`apiVersion: okdev.io/v1alpha1
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
`)
	raw, err := appendWorkloadProfileToConfigBytes(original, config.WorkloadProfile{
		Name: "train", Type: "job", ManifestPath: "job.yaml",
	})
	if err != nil {
		t.Fatalf("appendWorkloadProfileToConfigBytes: %v", err)
	}
	cfg, _, err := config.LoadFromBytes(raw, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("the rewritten config must still load: %v\n%s", err, raw)
	}
	names := cfg.WorkloadProfileNames()
	if len(names) != 2 || names[0] != config.DefaultWorkloadProfileName || names[1] != "train" {
		t.Fatalf("profiles = %v, want [default train]\n%s", names, raw)
	}
	if err := cfg.SelectWorkload(config.DefaultWorkloadProfileName); err != nil {
		t.Fatal(err)
	}
	if cfg.Spec.Workload.Type != "pod" || cfg.Spec.Workload.ManifestPath != "" {
		t.Fatalf("the implicit pod workload was not preserved: %+v\n%s", cfg.Spec.Workload, raw)
	}
	// It must still be a valid config: the pod profile keeps spec.podTemplate.
	if err := cfg.Validate(); err != nil {
		t.Fatalf("rewritten config must validate: %v\n%s", err, raw)
	}
}

// labelsForSession is the map stamped onto pods okdev creates;
// discoveryLabelsForSession is only used to find existing ones. The profile
// label has to be on the former or switch detection never fires.
