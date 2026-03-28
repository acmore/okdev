package cli

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
)

type fakeWorkloadExistenceChecker struct {
	exists     bool
	err        error
	apiVersion string
	kind       string
	name       string
}

func (f *fakeWorkloadExistenceChecker) ResourceExists(_ context.Context, _ string, apiVersion, kind, name string) (bool, error) {
	f.apiVersion = apiVersion
	f.kind = kind
	f.name = name
	return f.exists, f.err
}

type fakeRefRuntime struct {
	kind       string
	apiVersion string
	name       string
	refErr     error
}

func (f fakeRefRuntime) Kind() string                                              { return f.kind }
func (f fakeRefRuntime) WorkloadName() string                                      { return f.name }
func (f fakeRefRuntime) Apply(context.Context, workload.ApplyClient, string) error { return nil }
func (f fakeRefRuntime) Delete(context.Context, workload.DeleteClient, string, bool) error {
	return nil
}
func (f fakeRefRuntime) WaitReady(context.Context, workload.WaitClient, string, time.Duration, func(kube.PodReadinessProgress)) error {
	return nil
}
func (f fakeRefRuntime) SelectTarget(context.Context, workload.TargetClient, string) (workload.TargetRef, error) {
	return workload.TargetRef{}, nil
}
func (f fakeRefRuntime) WorkloadRef() (string, string, string, error) {
	return f.apiVersion, f.kind, f.name, f.refErr
}

func TestShouldReuseExistingWorkloadForExistingControllerBackedRuntime(t *testing.T) {
	k := &fakeWorkloadExistenceChecker{exists: true}
	rt := fakeRefRuntime{kind: workload.TypeJob, apiVersion: "batch/v1", name: "trainer"}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", rt, false)
	if err != nil {
		t.Fatalf("shouldReuseExistingWorkload: %v", err)
	}
	if !reuse {
		t.Fatal("expected reuse")
	}
	if k.apiVersion != "batch/v1" || k.kind != workload.TypeJob || k.name != "trainer" {
		t.Fatalf("unexpected workload ref lookup: %#v", k)
	}
}

func TestShouldReuseExistingWorkloadReusesExistingPodAndSkipsForReconcile(t *testing.T) {
	k := &fakeWorkloadExistenceChecker{exists: true}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", fakeRefRuntime{kind: workload.TypePod, apiVersion: "v1", name: "okdev-sess"}, false)
	if err != nil {
		t.Fatalf("pod shouldReuseExistingWorkload: %v", err)
	}
	if !reuse {
		t.Fatal("expected pod runtime to reuse existing workload")
	}
	if k.apiVersion != "v1" || k.kind != workload.TypePod || k.name != "okdev-sess" {
		t.Fatalf("unexpected pod workload ref lookup: %#v", k)
	}

	reuse, err = shouldReuseExistingWorkload(context.Background(), k, "default", fakeRefRuntime{kind: workload.TypeJob, apiVersion: "batch/v1", name: "trainer"}, true)
	if err != nil {
		t.Fatalf("reconcile shouldReuseExistingWorkload: %v", err)
	}
	if reuse {
		t.Fatal("expected reconcile mode to bypass reuse")
	}
}

func TestShouldReuseExistingWorkloadSurfacesLookupErrors(t *testing.T) {
	want := errors.New("boom")
	k := &fakeWorkloadExistenceChecker{err: want}
	rt := fakeRefRuntime{kind: workload.TypeGeneric, apiVersion: "apps/v1", name: "trainer"}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", rt, false)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected wrapped lookup error, got reuse=%v err=%v", reuse, err)
	}
}
