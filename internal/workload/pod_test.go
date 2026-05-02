package workload

import (
	"context"
	"strings"
	"testing"
	"time"

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

func TestPodRuntimeLifecycle(t *testing.T) {
	rt := NewPodRuntime("test",
		map[string]string{"okdev.io/managed": "true"}, nil,
		corev1.PodSpec{},
		[]corev1.Volume{{
			Name:         "workspace",
			VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
		}},
		"/workspace", "ghcr.io/acmore/okdev:edge",
		corev1.ResourceRequirements{}, false, "", "", "",
	)
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

	wait := &fakeWaitClient{}
	if err := rt.WaitReady(context.Background(), wait, "default", 30*time.Second, nil); err != nil {
		t.Fatalf("WaitReady: %v", err)
	}
	if wait.pod != "okdev-test" || wait.timeout != 30*time.Second {
		t.Fatalf("unexpected wait call: %+v", wait)
	}

	target, err := rt.SelectTarget(context.Background(), &fakeTargetClient{}, "default")
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
	if del.kind != "pod" || del.name != "okdev-test" || !del.ignore {
		t.Fatalf("unexpected delete call: %+v", del)
	}
}

func TestPodRuntimeSelectTargetUsesConfiguredContainer(t *testing.T) {
	rt := NewPodRuntime("test",
		map[string]string{"okdev.io/managed": "true"}, nil,
		corev1.PodSpec{},
		nil, "/workspace", "ghcr.io/acmore/okdev:edge",
		corev1.ResourceRequirements{}, false, "", "", "trainer",
	)
	target, err := rt.SelectTarget(context.Background(), &fakeTargetClient{}, "default")
	if err != nil {
		t.Fatalf("SelectTarget: %v", err)
	}
	if target.Container != "trainer" {
		t.Fatalf("expected container trainer, got %q", target.Container)
	}
}
