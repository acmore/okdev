package workload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// okdev picks the dev container by name, falling back to the first one when the
// name is absent. Injection did that; exec did not, so a manifest whose only
// container is not named "dev" built a working pod you could not get a shell
// into. Both sides must resolve to the same container.
func TestExecTargetsTheContainerOkdevInjectedInto(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "pod.yaml")
	if err := os.WriteFile(path, []byte(`apiVersion: v1
kind: Pod
metadata:
  name: okdev-sess
spec:
  containers:
    - name: trainer
      image: alpine
`), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &GenericRuntime{
		SessionName:        "sess",
		WorkloadKind:       TypePod,
		ManifestPath:       path,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		Inject:             []config.WorkloadInjectSpec{{Path: ""}},
		// TargetContainer deliberately unset: no attach.container in the config.
	}

	k := &fakeApplyClient{}
	if err := rt.Apply(t.Context(), k, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(k.manifest, &pod); err != nil {
		t.Fatal(err)
	}
	// Which container did okdev treat as the dev container? The one that got
	// OKDEV_CONTAINER_ROLE=dev.
	injected := ""
	for _, c := range pod.Spec.Containers {
		mounts := []string{}
		for _, vm := range c.VolumeMounts {
			mounts = append(mounts, vm.Name)
		}
		role := ""
		for _, e := range c.Env {
			if e.Name == "OKDEV_CONTAINER_ROLE" {
				role = e.Value
			}
		}
		t.Logf("container %-14q mounts=%v role=%q", c.Name, mounts, role)
		if role == "dev" {
			injected = c.Name
		}
	}
	// interactiveContainer is what SelectTarget puts in TargetRef.Container,
	// which is what okdev ssh/exec use.
	execTarget := rt.interactiveContainer()
	t.Logf("workspace mount injected into: %q", injected)
	t.Logf("exec/ssh would target:         %q", execTarget)
	if injected != execTarget {
		t.Fatalf("SPLIT CONFIRMED: injection targets %q but exec targets %q", injected, execTarget)
	}
}
