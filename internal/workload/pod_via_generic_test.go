package workload

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
)

type fakeApplyClient struct {
	namespace string
	manifest  []byte
}

func (f *fakeApplyClient) Apply(_ context.Context, namespace string, manifest []byte) error {
	f.namespace = namespace
	f.manifest = manifest
	return nil
}

type fakeDeleteClient struct {
	namespace string
	kind      string
	name      string
	ignore    bool
}

func (f *fakeDeleteClient) Delete(_ context.Context, namespace string, kind string, name string, ignoreNotFound bool) error {
	f.namespace = namespace
	f.kind = kind
	f.name = name
	f.ignore = ignoreNotFound
	return nil
}

func (f *fakeDeleteClient) DeleteByRef(_ context.Context, namespace string, _ string, kind string, name string, ignoreNotFound bool) error {
	return f.Delete(context.Background(), namespace, kind, name, ignoreNotFound)
}

type fakeWaitClient struct {
	namespace string
	pod       string
	timeout   time.Duration
}

func (f *fakeWaitClient) WaitReadyWithProgress(_ context.Context, namespace, pod string, timeout time.Duration, _ func(kube.PodReadinessProgress)) error {
	f.namespace = namespace
	f.pod = pod
	f.timeout = timeout
	return nil
}

func (f *fakeWaitClient) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return nil, nil
}

type fakeTargetClient struct{}

func (f *fakeTargetClient) GetPodSummary(_ context.Context, namespace, name string) (*kube.PodSummary, error) {
	return &kube.PodSummary{Name: name, Namespace: namespace}, nil
}

func (f *fakeTargetClient) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	return nil, nil
}

// podRuntimeForTest builds the runtime a pod workload gets today: a
// GenericRuntime over a synthesized Pod manifest, injected at the object root.
func podRuntimeForTest(sessionName, targetContainer string, volumes []corev1.Volume) *GenericRuntime {
	return &GenericRuntime{
		SessionName:  sessionName,
		WorkloadKind: TypePod,
		ManifestBytes: []byte(`
apiVersion: v1
kind: Pod
metadata:
  name: okdev-` + sessionName + `
spec:
  containers:
    - name: dev
      image: ubuntu:22.04
`),
		WorkspaceMountPath: "/workspace",
		SidecarImage:       "ghcr.io/acmore/okdev:edge",
		SidecarResources:   corev1.ResourceRequirements{},
		TargetContainer:    targetContainer,
		Volumes:            volumes,
		Labels:             map[string]string{"okdev.io/managed": "true"},
		Inject:             []config.WorkloadInjectSpec{{Path: ""}},
	}
}

func attachablePod(name string) kube.PodSummary {
	return kube.PodSummary{
		Name:      name,
		Phase:     "Running",
		Ready:     "1/1",
		CreatedAt: time.Now(),
		Labels:    map[string]string{"okdev.io/attachable": "true"},
	}
}

func TestPodViaGenericLifecycle(t *testing.T) {
	rt := podRuntimeForTest("test", "", []corev1.Volume{{
		Name:         "workspace",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}})
	if rt.Kind() != TypePod {
		t.Fatalf("unexpected kind: %s", rt.Kind())
	}
	if rt.WorkloadName() != "okdev-test" {
		t.Fatalf("unexpected workload name: %s", rt.WorkloadName())
	}

	apply := &fakeApplyClient{}
	if err := rt.Apply(context.Background(), apply, "default"); err != nil {
		t.Fatalf("Apply: %v", err)
	}
	if apply.namespace != "default" || len(apply.manifest) == 0 {
		t.Fatalf("unexpected apply call: %+v", apply)
	}
	// Verify that Apply prepared the spec (sidecar should be present)
	manifest := string(apply.manifest)
	if !strings.Contains(manifest, "okdev-sidecar") {
		t.Fatal("expected prepared manifest to contain okdev-sidecar container")
	}
	// The root inject path must not clobber apiVersion/kind.
	if !strings.Contains(manifest, "kind: Pod") {
		t.Fatalf("root inject dropped the Pod type meta:\n%s", manifest)
	}

	client := &fakeGenericClient{pods: []kube.PodSummary{attachablePod("okdev-test")}}
	if err := rt.WaitReady(context.Background(), client, "default", 30*time.Second, nil); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if client.pod != "okdev-test" {
		t.Fatalf("unexpected wait call: %+v", client)
	}

	target, err := rt.SelectTarget(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.PodName != "okdev-test" || target.Container != DefaultTargetContainer {
		t.Fatalf("unexpected target: %+v", target)
	}

	del := &fakeDeleteClient{}
	if err := rt.Delete(context.Background(), del, "default", true); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	// Generic deletes go through DeleteByRef, which carries the object's own
	// kind ("Pod") rather than the lowercase resource name.
	if del.kind != "Pod" || del.name != "okdev-test" || !del.ignore {
		t.Fatalf("unexpected delete call: %+v", del)
	}
}

func TestPodViaGenericSelectTargetUsesConfiguredContainer(t *testing.T) {
	rt := podRuntimeForTest("test", "trainer", nil)
	client := &fakeGenericClient{pods: []kube.PodSummary{attachablePod("okdev-test")}}
	target, err := rt.SelectTarget(context.Background(), client, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.Container != "trainer" {
		t.Fatalf("expected container trainer, got %q", target.Container)
	}
}
