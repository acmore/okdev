package workload

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

func applyManifest(t *testing.T, body string) corev1.Pod {
	t.Helper()
	path := filepath.Join(t.TempDir(), "pod.yaml")
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	rt := &GenericRuntime{
		SessionName:        "sess",
		WorkloadKind:       TypePod,
		ManifestPath:       path,
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		Inject:             []config.WorkloadInjectSpec{{Path: ""}},
	}
	k := &fakeApplyClient{}
	if err := rt.Apply(t.Context(), k, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	var pod corev1.Pod
	if err := yaml.Unmarshal(k.manifest, &pod); err != nil {
		t.Fatal(err)
	}
	return pod
}

func volumeNamed(pod corev1.Pod, name string) *corev1.Volume {
	for i := range pod.Spec.Volumes {
		if pod.Spec.Volumes[i].Name == name {
			return &pod.Spec.Volumes[i]
		}
	}
	return nil
}

// The one place okdev still writes a volume into the manifest: the sync target,
// and only when the manifest declares none.
func TestWorkspaceIsInjectedOnlyWhenTheManifestDeclaresNone(t *testing.T) {
	pod := applyManifest(t, `apiVersion: v1
kind: Pod
metadata:
  name: okdev-sess
spec:
  containers:
    - name: dev
      image: alpine
      volumeMounts:
        - name: workspace
          mountPath: /workspace
`)
	ws := volumeNamed(pod, "workspace")
	if ws == nil || ws.EmptyDir == nil {
		t.Fatalf("a manifest declaring no workspace must get one: %+v", pod.Spec.Volumes)
	}
	// The sidecar's own state is okdev's, not the manifest's, and is injected
	// either way.
	for _, name := range []string{"syncthing-home", "okdev-runtime"} {
		if volumeNamed(pod, name) == nil {
			t.Fatalf("%s must be injected: %+v", name, pod.Spec.Volumes)
		}
	}
}

// A manifest that declares workspace itself keeps exactly what it declared.
// okdev substituting its own emptyDir here is the defect this change removes.
func TestADeclaredWorkspaceVolumeIsLeftAlone(t *testing.T) {
	pod := applyManifest(t, `apiVersion: v1
kind: Pod
metadata:
  name: okdev-sess
spec:
  containers:
    - name: dev
      image: alpine
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: team-ws
`)
	ws := volumeNamed(pod, "workspace")
	if ws == nil || ws.PersistentVolumeClaim == nil {
		t.Fatalf("the manifest's workspace PVC must survive: %+v", pod.Spec.Volumes)
	}
	if ws.PersistentVolumeClaim.ClaimName != "team-ws" {
		t.Fatalf("claim = %q, want team-ws", ws.PersistentVolumeClaim.ClaimName)
	}
}

// Volumes the manifest declares that okdev knows nothing about are untouched.
func TestManifestVolumesAreNotRewritten(t *testing.T) {
	pod := applyManifest(t, `apiVersion: v1
kind: Pod
metadata:
  name: okdev-sess
spec:
  containers:
    - name: dev
      image: alpine
      volumeMounts:
        - name: datasets
          mountPath: /data
  volumes:
    - name: datasets
      persistentVolumeClaim:
        claimName: shared-ds
`)
	ds := volumeNamed(pod, "datasets")
	if ds == nil || ds.PersistentVolumeClaim == nil || ds.PersistentVolumeClaim.ClaimName != "shared-ds" {
		t.Fatalf("a manifest volume must be untouched: %+v", pod.Spec.Volumes)
	}
}
