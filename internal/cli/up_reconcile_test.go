package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	corev1 "k8s.io/api/core/v1"
)

type fakeWorkloadExistenceChecker struct {
	exists     bool
	err        error
	apiVersion string
	kind       string
	name       string
	podsSeq    [][]kube.PodSummary
	listCalls  int
}

func TestResolveRunWorkloadIdentityReusesExistingSavedWorkload(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveInfo(session.Info{
		Name:               "sess",
		RunID:              "abcd1234",
		WorkloadAPIVersion: "v1",
		WorkloadKind:       "Pod",
		WorkloadName:       "okdev-sess-abcd1234",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}
	k := &fakeWorkloadExistenceChecker{exists: true}

	runID, workloadName, err := resolveRunWorkloadIdentity(context.Background(), k, "default", "sess")
	if err != nil {
		t.Fatalf("resolveRunWorkloadIdentity: %v", err)
	}
	if runID != "abcd1234" || workloadName != "okdev-sess-abcd1234" {
		t.Fatalf("expected saved identity, got runID=%q workload=%q", runID, workloadName)
	}
	if k.apiVersion != "v1" || k.kind != "Pod" || k.name != "okdev-sess-abcd1234" {
		t.Fatalf("unexpected saved ref lookup: %#v", k)
	}
}

func TestResolveRunWorkloadIdentityCreatesNewNameWhenSavedWorkloadIsAbsent(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveInfo(session.Info{
		Name:               "sess",
		RunID:              "abcd1234",
		WorkloadAPIVersion: "v1",
		WorkloadKind:       "Pod",
		WorkloadName:       "okdev-sess-abcd1234",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}
	k := &fakeWorkloadExistenceChecker{exists: false}

	runID, workloadName, err := resolveRunWorkloadIdentity(context.Background(), k, "default", "sess")
	if err != nil {
		t.Fatalf("resolveRunWorkloadIdentity: %v", err)
	}
	if runID == "abcd1234" || workloadName == "okdev-sess-abcd1234" {
		t.Fatalf("expected new identity, got runID=%q workload=%q", runID, workloadName)
	}
	if !strings.HasPrefix(workloadName, "okdev-sess-") {
		t.Fatalf("expected generated workload name to include session, got %q", workloadName)
	}
}

func TestWorkloadNameForRunFitsDNSLabelLimit(t *testing.T) {
	name := workloadNameForRun(strings.Repeat("a", 80), "abcd1234")
	if len(name) > 63 {
		t.Fatalf("expected name <= 63 chars, got %d: %q", len(name), name)
	}
	if !strings.HasPrefix(name, "okdev-") || !strings.HasSuffix(name, "-abcd1234") {
		t.Fatalf("unexpected workload name %q", name)
	}
}

func (f *fakeWorkloadExistenceChecker) ResourceExists(_ context.Context, _ string, apiVersion, kind, name string) (bool, error) {
	f.apiVersion = apiVersion
	f.kind = kind
	f.name = name
	return f.exists, f.err
}

func (f *fakeWorkloadExistenceChecker) ListPods(_ context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	if len(f.podsSeq) == 0 {
		return nil, f.err
	}
	idx := f.listCalls
	if idx >= len(f.podsSeq) {
		idx = len(f.podsSeq) - 1
	}
	f.listCalls++
	return f.podsSeq[idx], f.err
}

func (f *fakeWorkloadExistenceChecker) GetPodSummary(_ context.Context, _ string, name string) (*kube.PodSummary, error) {
	if f.err != nil {
		return nil, f.err
	}
	if len(f.podsSeq) == 0 {
		return &kube.PodSummary{Name: name}, nil
	}
	idx := f.listCalls
	if idx >= len(f.podsSeq) {
		idx = len(f.podsSeq) - 1
	}
	for _, pod := range f.podsSeq[idx] {
		if pod.Name == name {
			p := pod
			return &p, nil
		}
	}
	return &kube.PodSummary{Name: name}, nil
}

type fakeRefRuntime struct {
	kind       string
	apiVersion string
	name       string
	refErr     error
	deleteErr  error
	deleteCall int
}

type fakeSetupExecClient struct {
	errs  []error
	calls []workload.TargetRef
}

func (f *fakeSetupExecClient) ExecShInContainer(_ context.Context, _ string, pod, container, _ string) ([]byte, error) {
	f.calls = append(f.calls, workload.TargetRef{PodName: pod, Container: container})
	if len(f.errs) == 0 {
		return nil, nil
	}
	err := f.errs[0]
	f.errs = f.errs[1:]
	return nil, err
}

func (f *fakeRefRuntime) Kind() string                                              { return f.kind }
func (f *fakeRefRuntime) WorkloadName() string                                      { return f.name }
func (f *fakeRefRuntime) Apply(context.Context, workload.ApplyClient, string) error { return nil }
func (f *fakeRefRuntime) Delete(context.Context, workload.DeleteClient, string, bool) error {
	f.deleteCall++
	return f.deleteErr
}
func (f *fakeRefRuntime) WaitReady(context.Context, workload.WaitClient, string, time.Duration, func(kube.PodReadinessProgress)) error {
	return nil
}
func (f *fakeRefRuntime) SelectTarget(context.Context, workload.TargetClient, string) (workload.TargetRef, error) {
	return workload.TargetRef{}, nil
}
func (f *fakeRefRuntime) WorkloadRef() (string, string, string, error) {
	return f.apiVersion, f.kind, f.name, f.refErr
}

func TestShouldReuseExistingWorkloadForExistingControllerBackedRuntime(t *testing.T) {
	k := &fakeWorkloadExistenceChecker{exists: true}
	rt := &fakeRefRuntime{kind: workload.TypeJob, apiVersion: "batch/v1", name: "trainer"}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", rt)
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
	k := &fakeWorkloadExistenceChecker{
		exists: true,
		podsSeq: [][]kube.PodSummary{{
			{Name: "okdev-sess", Phase: string(corev1.PodRunning)},
		}},
	}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", &fakeRefRuntime{kind: "Pod", apiVersion: "v1", name: "okdev-sess"})
	if err != nil {
		t.Fatalf("pod shouldReuseExistingWorkload: %v", err)
	}
	if !reuse {
		t.Fatal("expected pod runtime to reuse existing workload")
	}
	if k.apiVersion != "v1" || k.kind != "Pod" || k.name != "okdev-sess" {
		t.Fatalf("unexpected pod workload ref lookup: %#v", k)
	}

	reuse, err = shouldReuseExistingWorkload(context.Background(), k, "default", &fakeRefRuntime{kind: workload.TypeJob, apiVersion: "batch/v1", name: "trainer"})
	if err != nil {
		t.Fatalf("controller shouldReuseExistingWorkload: %v", err)
	}
	if !reuse {
		t.Fatal("expected existing controller-backed workload to be detected")
	}
}

func TestShouldReuseExistingWorkloadSkipsTerminatingPod(t *testing.T) {
	k := &fakeWorkloadExistenceChecker{
		exists: true,
		podsSeq: [][]kube.PodSummary{{
			{Name: "okdev-sess", Phase: string(corev1.PodRunning), Deleting: true},
		}},
	}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", &fakeRefRuntime{kind: "Pod", apiVersion: "v1", name: "okdev-sess"})
	if err != nil {
		t.Fatalf("pod shouldReuseExistingWorkload: %v", err)
	}
	if reuse {
		t.Fatal("expected terminating pod runtime to skip reuse")
	}
}

func TestShouldReuseExistingWorkloadSkipsTerminalPod(t *testing.T) {
	k := &fakeWorkloadExistenceChecker{
		exists: true,
		podsSeq: [][]kube.PodSummary{{
			{Name: "okdev-sess", Phase: string(corev1.PodSucceeded)},
		}},
	}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", &fakeRefRuntime{kind: "Pod", apiVersion: "v1", name: "okdev-sess"})
	if err != nil {
		t.Fatalf("pod shouldReuseExistingWorkload: %v", err)
	}
	if reuse {
		t.Fatal("expected terminal pod runtime to skip reuse")
	}
}

// Regression: PodRuntime.WorkloadRef() returns API kind "Pod" (capital P),
// while workload.TypePod is "pod" (lowercase). The reuse check must handle
// both casings so that terminating/terminal pod checks are not skipped.
func TestShouldReuseExistingWorkloadSkipsTerminatingPodAPIKind(t *testing.T) {
	for _, kind := range []string{"Pod", "pod", "POD"} {
		t.Run(kind, func(t *testing.T) {
			k := &fakeWorkloadExistenceChecker{
				exists: true,
				podsSeq: [][]kube.PodSummary{{
					{Name: "okdev-sess", Phase: string(corev1.PodRunning), Deleting: true},
				}},
			}
			reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default",
				&fakeRefRuntime{kind: kind, apiVersion: "v1", name: "okdev-sess"})
			if err != nil {
				t.Fatalf("shouldReuseExistingWorkload(%q): %v", kind, err)
			}
			if reuse {
				t.Fatalf("expected terminating pod (kind=%q) to skip reuse", kind)
			}
		})
	}
}

func TestShouldReuseExistingWorkloadSkipsTerminalPodAPIKind(t *testing.T) {
	for _, kind := range []string{"Pod", "pod"} {
		for _, phase := range []corev1.PodPhase{corev1.PodSucceeded, corev1.PodFailed} {
			t.Run(kind+"/"+string(phase), func(t *testing.T) {
				k := &fakeWorkloadExistenceChecker{
					exists: true,
					podsSeq: [][]kube.PodSummary{{
						{Name: "okdev-sess", Phase: string(phase)},
					}},
				}
				reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default",
					&fakeRefRuntime{kind: kind, apiVersion: "v1", name: "okdev-sess"})
				if err != nil {
					t.Fatalf("shouldReuseExistingWorkload(%q, %s): %v", kind, phase, err)
				}
				if reuse {
					t.Fatalf("expected terminal pod (kind=%q, phase=%s) to skip reuse", kind, phase)
				}
			})
		}
	}
}

func TestShouldReuseExistingWorkloadSurfacesLookupErrors(t *testing.T) {
	want := errors.New("boom")
	k := &fakeWorkloadExistenceChecker{err: want}
	rt := &fakeRefRuntime{kind: workload.TypeGeneric, apiVersion: "apps/v1", name: "trainer"}

	reuse, err := shouldReuseExistingWorkload(context.Background(), k, "default", rt)
	if err == nil || !errors.Is(err, want) {
		t.Fatalf("expected wrapped lookup error, got reuse=%v err=%v", reuse, err)
	}
}

func TestPrepareReconcileApplyRecreatesExistingPod(t *testing.T) {
	rt := &fakeRefRuntime{kind: workload.TypePod, apiVersion: "v1", name: "okdev-sess"}
	k := &fakeWorkloadExistenceChecker{
		exists:  false,
		podsSeq: [][]kube.PodSummary{{}},
	}
	state := &upState{
		ctx:           context.Background(),
		runtime:       rt,
		reconcileKube: k,
		labels:        map[string]string{"okdev.io/session": "sess"},
		command: &commandContext{
			namespace:   "default",
			kube:        &kube.Client{},
			sessionName: "sess",
		},
	}

	outcome, err := prepareReconcileApply(state)
	if err != nil {
		t.Fatalf("prepareReconcileApply: %v", err)
	}
	if outcome != workloadApplyRecreated {
		t.Fatalf("expected recreate outcome, got %v", outcome)
	}
	if rt.deleteCall != 1 {
		t.Fatalf("expected one delete call, got %d", rt.deleteCall)
	}
}

func TestPrepareReconcileApplyReappliesControllerWorkload(t *testing.T) {
	rt := &fakeRefRuntime{kind: workload.TypeGeneric, apiVersion: "apps/v1", name: "trainer"}
	state := &upState{
		runtime: rt,
		command: &commandContext{},
	}

	outcome, err := prepareReconcileApply(state)
	if err != nil {
		t.Fatalf("prepareReconcileApply: %v", err)
	}
	if outcome != workloadApplyReapplied {
		t.Fatalf("expected reapply outcome, got %v", outcome)
	}
	if rt.deleteCall != 0 {
		t.Fatalf("expected no delete calls, got %d", rt.deleteCall)
	}
}

func TestPrepareReconcileApplyRecreatesJobWorkload(t *testing.T) {
	rt := &fakeRefRuntime{kind: workload.TypeJob, apiVersion: "batch/v1", name: "trainer"}
	k := &fakeWorkloadExistenceChecker{
		exists:  false,
		podsSeq: [][]kube.PodSummary{{}},
	}
	state := &upState{
		ctx:           context.Background(),
		runtime:       rt,
		reconcileKube: k,
		labels:        map[string]string{"okdev.io/session": "sess"},
		command: &commandContext{
			namespace:   "default",
			kube:        &kube.Client{},
			sessionName: "sess",
		},
	}

	outcome, err := prepareReconcileApply(state)
	if err != nil {
		t.Fatalf("prepareReconcileApply: %v", err)
	}
	if outcome != workloadApplyRecreated {
		t.Fatalf("expected recreate outcome, got %v", outcome)
	}
	if rt.deleteCall != 1 {
		t.Fatalf("expected one delete call, got %d", rt.deleteCall)
	}
}

func TestPrepareReconcileApplyRecreatesPyTorchJobWorkload(t *testing.T) {
	rt := &fakeRefRuntime{kind: workload.TypePyTorchJob, apiVersion: "kubeflow.org/v1", name: "trainer"}
	k := &fakeWorkloadExistenceChecker{
		exists:  false,
		podsSeq: [][]kube.PodSummary{{}},
	}
	state := &upState{
		ctx:           context.Background(),
		runtime:       rt,
		reconcileKube: k,
		labels:        map[string]string{"okdev.io/session": "sess"},
		command: &commandContext{
			namespace:   "default",
			kube:        &kube.Client{},
			sessionName: "sess",
		},
	}

	outcome, err := prepareReconcileApply(state)
	if err != nil {
		t.Fatalf("prepareReconcileApply: %v", err)
	}
	if outcome != workloadApplyRecreated {
		t.Fatalf("expected recreate outcome, got %v", outcome)
	}
	if rt.deleteCall != 1 {
		t.Fatalf("expected one delete call, got %d", rt.deleteCall)
	}
}

func TestPrepareReconcileApplyWaitsForDeletionBarrier(t *testing.T) {
	rt := &fakeRefRuntime{kind: workload.TypePyTorchJob, apiVersion: "kubeflow.org/v1", name: "trainer"}
	k := &fakeWorkloadExistenceChecker{
		exists: false,
		podsSeq: [][]kube.PodSummary{
			{{Name: "pod-a"}},
			{},
		},
	}
	state := &upState{
		ctx:           context.Background(),
		runtime:       rt,
		reconcileKube: k,
		labels:        map[string]string{"okdev.io/session": "sess"},
		command: &commandContext{
			namespace:   "default",
			sessionName: "sess",
			kube:        &kube.Client{},
		},
	}

	outcome, err := prepareReconcileApply(state)
	if err != nil {
		t.Fatalf("prepareReconcileApply: %v", err)
	}
	if outcome != workloadApplyRecreated {
		t.Fatalf("expected recreate outcome, got %v", outcome)
	}
	if k.listCalls < 2 {
		t.Fatalf("expected deletion barrier to poll pods, got %d list calls", k.listCalls)
	}
}

func TestEnsureCompatibleExistingSessionWorkloadAllowsSameType(t *testing.T) {
	err := ensureCompatibleExistingSessionWorkload(context.Background(), fakeSessionAccessReader{
		pods: []kube.PodSummary{{
			Name:   "okdev-sess-a",
			Labels: map[string]string{"okdev.io/workload-type": workload.TypeJob},
		}},
	}, "default", "sess-a", workload.TypeJob)
	if err != nil {
		t.Fatalf("expected matching workload type to be allowed, got %v", err)
	}
}

func TestEnsureCompatibleExistingSessionWorkloadTreatsEmptyTypeAsPod(t *testing.T) {
	err := ensureCompatibleExistingSessionWorkload(context.Background(), fakeSessionAccessReader{
		pods: []kube.PodSummary{{
			Name:   "okdev-sess-a",
			Labels: map[string]string{},
		}},
	}, "default", "sess-a", "")
	if err != nil {
		t.Fatalf("expected empty workload type to normalize to pod, got %v", err)
	}
}

func TestEnsureCompatibleExistingSessionWorkloadRejectsConflictingType(t *testing.T) {
	err := ensureCompatibleExistingSessionWorkload(context.Background(), fakeSessionAccessReader{
		pods: []kube.PodSummary{{
			Name:   "okdev-sess-a",
			Labels: map[string]string{"okdev.io/workload-type": workload.TypePod},
		}},
	}, "default", "sess-a", workload.TypeJob)
	if err == nil || !strings.Contains(err.Error(), `already exists in namespace "default" with workload type "pod"; current config expects "job"`) {
		t.Fatalf("expected workload conflict error, got %v", err)
	}
}

func TestEnsureCompatibleExistingSessionWorkloadRejectsMixedExistingTypes(t *testing.T) {
	err := ensureCompatibleExistingSessionWorkload(context.Background(), fakeSessionAccessReader{
		pods: []kube.PodSummary{
			{Name: "okdev-sess-a", Labels: map[string]string{"okdev.io/workload-type": workload.TypePod}},
			{Name: "okdev-hx2n9", Labels: map[string]string{"okdev.io/workload-type": workload.TypeJob}},
		},
	}, "default", "sess-a", workload.TypeJob)
	if err == nil || !strings.Contains(err.Error(), `multiple workload types (job, pod)`) {
		t.Fatalf("expected mixed workload type error, got %v", err)
	}
}

func TestRetryTargetStepRefreshesOnTransientTargetError(t *testing.T) {
	start := workload.TargetRef{PodName: "old-pod", Container: "dev"}
	refreshed := workload.TargetRef{PodName: "new-pod", Container: "dev"}
	stepCalls := 0
	refreshCalls := 0

	got, err := retryTargetStep(start, shouldRetrySetupTargetError, func(current workload.TargetRef) (workload.TargetRef, error) {
		refreshCalls++
		if current.PodName != "old-pod" {
			t.Fatalf("expected refresh to start from old target, got %q", current.PodName)
		}
		return refreshed, nil
	}, func(current workload.TargetRef) error {
		stepCalls++
		if stepCalls == 1 {
			if current.PodName != "old-pod" {
				t.Fatalf("expected first attempt on old target, got %q", current.PodName)
			}
			return errors.New(`install ssh key in pod: unable to upgrade connection: container not found ("dev")`)
		}
		if current.PodName != "new-pod" {
			t.Fatalf("expected retry on refreshed target, got %q", current.PodName)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("retryTargetStep: %v", err)
	}
	if got.PodName != "new-pod" {
		t.Fatalf("expected refreshed target, got %q", got.PodName)
	}
	if stepCalls != 2 {
		t.Fatalf("expected 2 step attempts, got %d", stepCalls)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCalls)
	}
}

func TestRetryTargetStepDoesNotRefreshOnPermanentError(t *testing.T) {
	start := workload.TargetRef{PodName: "old-pod", Container: "dev"}
	refreshCalls := 0

	got, err := retryTargetStep(start, shouldRetrySetupTargetError, func(workload.TargetRef) (workload.TargetRef, error) {
		refreshCalls++
		return workload.TargetRef{}, nil
	}, func(current workload.TargetRef) error {
		if current.PodName != "old-pod" {
			t.Fatalf("expected original target, got %q", current.PodName)
		}
		return errors.New("permission denied")
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permanent error, got %v", err)
	}
	if got.PodName != "old-pod" {
		t.Fatalf("expected original target, got %q", got.PodName)
	}
	if refreshCalls != 0 {
		t.Fatalf("expected no refreshes, got %d", refreshCalls)
	}
}

func TestWaitForTargetExecReadyRefreshesOnTransientExecError(t *testing.T) {
	client := &fakeSetupExecClient{
		errs: []error{
			errors.New(`unable to upgrade connection: container not found ("dev")`),
			nil,
		},
	}
	start := workload.TargetRef{PodName: "old-pod", Container: "dev"}
	refreshed := workload.TargetRef{PodName: "new-pod", Container: "dev"}
	refreshCalls := 0

	got, err := waitForTargetExecReady(context.Background(), client, "default", start, func(current workload.TargetRef) (workload.TargetRef, error) {
		refreshCalls++
		if current.PodName != "old-pod" {
			t.Fatalf("expected refresh from old target, got %q", current.PodName)
		}
		return refreshed, nil
	})
	if err != nil {
		t.Fatalf("waitForTargetExecReady: %v", err)
	}
	if got.PodName != "new-pod" {
		t.Fatalf("expected refreshed target, got %q", got.PodName)
	}
	if refreshCalls != 1 {
		t.Fatalf("expected 1 refresh call, got %d", refreshCalls)
	}
	if len(client.calls) != 2 {
		t.Fatalf("expected 2 exec attempts, got %d", len(client.calls))
	}
	if client.calls[0].PodName != "old-pod" || client.calls[1].PodName != "new-pod" {
		t.Fatalf("unexpected exec attempt order: %#v", client.calls)
	}
}

func TestWaitForTargetExecReadyStopsOnPermanentExecError(t *testing.T) {
	client := &fakeSetupExecClient{
		errs: []error{errors.New("permission denied")},
	}
	start := workload.TargetRef{PodName: "old-pod", Container: "dev"}
	refreshCalls := 0

	got, err := waitForTargetExecReady(context.Background(), client, "default", start, func(workload.TargetRef) (workload.TargetRef, error) {
		refreshCalls++
		return workload.TargetRef{}, nil
	})
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permanent exec error, got %v", err)
	}
	if got.PodName != "old-pod" {
		t.Fatalf("expected original target, got %q", got.PodName)
	}
	if refreshCalls != 0 {
		t.Fatalf("expected no refresh call, got %d", refreshCalls)
	}
	if len(client.calls) != 1 || client.calls[0].PodName != "old-pod" {
		t.Fatalf("unexpected exec attempts: %#v", client.calls)
	}
}
