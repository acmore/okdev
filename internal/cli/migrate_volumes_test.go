package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

const volumesConfig = `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: proj
spec:
  namespace: default
  # the team's shared claims
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: team-ws
  sync:
    engine: syncthing
    paths:
      - ".:/workspace"
  workload:
    type: pod
    manifestPath: pod.yaml
`

const bareManifest = `apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: alpine
      volumeMounts:
        - name: workspace
          mountPath: /workspace
`

func TestVolumeMoveCarriesTheCommentWithTheVolumes(t *testing.T) {
	pending := []plannedManifest{{Path: "pod.yaml", Target: "/repo/.okdev/pod.yaml", Bytes: []byte(bareManifest)}}
	got, err := planVolumeMove("/repo/.okdev/okdev.yaml", []byte(volumesConfig), pending)
	if err != nil {
		t.Fatalf("planVolumeMove: %v", err)
	}
	if !got.Applied || len(got.Manifests) != 1 {
		t.Fatalf("expected one migrated manifest, got %+v", got.Manifests)
	}
	manifest := string(got.Manifests[0].Bytes)
	if !strings.Contains(manifest, "claimName: team-ws") {
		t.Fatalf("the volume must reach the manifest:\n%s", manifest)
	}
	// The comment described those volumes; leaving it behind would strand the
	// user's own note in a .bak they will never open.
	if !strings.Contains(manifest, "the team's shared claims") {
		t.Fatalf("the comment must travel with the volumes:\n%s", manifest)
	}
	cfg, _, err := config.LoadFromBytes(got.ConfigBytes, "/repo/.okdev/okdev.yaml")
	if err != nil {
		t.Fatalf("migrated config must load: %v\n%s", err, got.ConfigBytes)
	}
	if len(cfg.Spec.Volumes) != 0 {
		t.Fatalf("spec.volumes must be gone: %+v", cfg.Spec.Volumes)
	}
}

// A volume the manifest already declares stays as the manifest wrote it, and
// the config's copy is dropped with a warning — that shadowing was already
// happening, silently, and is the defect this change surfaces.
func TestVolumeMoveLeavesADeclaredVolumeAlone(t *testing.T) {
	declared := `apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: alpine
  volumes:
    - name: workspace
      emptyDir: {}
`
	pending := []plannedManifest{{Path: "pod.yaml", Target: "/repo/.okdev/pod.yaml", Bytes: []byte(declared)}}
	got, err := planVolumeMove("/repo/.okdev/okdev.yaml", []byte(volumesConfig), pending)
	if err != nil {
		t.Fatalf("planVolumeMove: %v", err)
	}
	manifest := string(got.Manifests[0].Bytes)
	if strings.Contains(manifest, "team-ws") {
		t.Fatalf("the manifest's own declaration must win:\n%s", manifest)
	}
	if len(got.Warnings) == 0 || !strings.Contains(strings.Join(got.Warnings, " "), "workspace") {
		t.Fatalf("dropping the config's copy must be reported, got %v", got.Warnings)
	}
}
