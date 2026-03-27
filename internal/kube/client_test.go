package kube

import (
	"errors"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestManualSelectorEnabled(t *testing.T) {
	if manualSelectorEnabled(nil) {
		t.Fatal("expected nil to be disabled")
	}
	f := false
	if manualSelectorEnabled(&f) {
		t.Fatal("expected false to be disabled")
	}
	tr := true
	if !manualSelectorEnabled(&tr) {
		t.Fatal("expected true to be enabled")
	}
}

func TestNormalizePodSpecDefaults(t *testing.T) {
	grace := int64(30)
	spec := corev1.PodSpec{
		TerminationGracePeriodSeconds: &grace,
		DNSPolicy:                     corev1.DNSClusterFirst,
		SchedulerName:                 "default-scheduler",
		SecurityContext:               &corev1.PodSecurityContext{},
		Containers: []corev1.Container{{
			Name:                     "dev",
			ImagePullPolicy:          corev1.PullIfNotPresent,
			TerminationMessagePath:   "/dev/termination-log",
			TerminationMessagePolicy: corev1.TerminationMessageReadFile,
			Ports:                    []corev1.ContainerPort{{ContainerPort: 8080, Protocol: corev1.ProtocolTCP}},
		}},
	}

	normalizePodSpecDefaults(&spec)

	if spec.TerminationGracePeriodSeconds != nil {
		t.Fatal("expected default termination grace period to be cleared")
	}
	if spec.DNSPolicy != "" {
		t.Fatalf("expected dns policy default to be cleared, got %q", spec.DNSPolicy)
	}
	if spec.SchedulerName != "" {
		t.Fatalf("expected scheduler default to be cleared, got %q", spec.SchedulerName)
	}
	if spec.SecurityContext != nil {
		t.Fatal("expected empty pod security context to be cleared")
	}
	container := spec.Containers[0]
	if container.ImagePullPolicy != "" || container.TerminationMessagePath != "" || container.TerminationMessagePolicy != "" {
		t.Fatalf("expected container defaults to be cleared: %+v", container)
	}
	if got := container.Ports[0].Protocol; got != "" {
		t.Fatalf("expected default TCP protocol to be cleared, got %q", got)
	}
}

func TestNormalizeJobForApplyComparison(t *testing.T) {
	parallelism := int32(1)
	completions := int32(1)
	manualSelector := false
	nonIndexed := batchv1.NonIndexedCompletion
	suspend := false
	policy := batchv1.TerminatingOrFailed
	job := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			ResourceVersion: "rv",
			UID:             "uid",
			CreationTimestamp: metav1.NewTime(
				time.Unix(1, 0),
			),
			Generation:    2,
			ManagedFields: []metav1.ManagedFieldsEntry{{Manager: "test"}},
			Labels:        map[string]string{"keep": "yes"},
		},
		Spec: batchv1.JobSpec{
			Parallelism:          &parallelism,
			Completions:          &completions,
			ManualSelector:       &manualSelector,
			CompletionMode:       &nonIndexed,
			Suspend:              &suspend,
			PodReplacementPolicy: &policy,
			Selector:             &metav1.LabelSelector{MatchLabels: map[string]string{"controller-uid": "x"}},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"batch.kubernetes.io/controller-uid": "x",
						"job-name":                           "demo",
						"keep":                               "yes",
					},
				},
				Spec: corev1.PodSpec{
					DNSPolicy:     corev1.DNSClusterFirst,
					SchedulerName: "default-scheduler",
					Containers:    []corev1.Container{{Name: "dev", Image: "alpine", ImagePullPolicy: corev1.PullIfNotPresent}},
				},
			},
		},
	}

	normalizeJobForApplyComparison(job)

	if job.ResourceVersion != "" || job.UID != "" || !job.CreationTimestamp.IsZero() || job.Generation != 0 || job.ManagedFields != nil {
		t.Fatalf("expected server-managed metadata to be cleared: %+v", job.ObjectMeta)
	}
	if job.Spec.Selector != nil {
		t.Fatal("expected selector to be cleared")
	}
	if job.Spec.Parallelism != nil || job.Spec.Completions != nil || job.Spec.ManualSelector != nil || job.Spec.CompletionMode != nil || job.Spec.Suspend != nil || job.Spec.PodReplacementPolicy != nil {
		t.Fatalf("expected defaulted job spec fields to be cleared: %+v", job.Spec)
	}
	if _, ok := job.Spec.Template.Labels["batch.kubernetes.io/controller-uid"]; ok {
		t.Fatal("expected controller uid label to be removed")
	}
	if _, ok := job.Spec.Template.Labels["job-name"]; ok {
		t.Fatal("expected job-name label to be removed")
	}
	if job.Spec.Template.Spec.DNSPolicy != "" || job.Spec.Template.Spec.SchedulerName != "" {
		t.Fatalf("expected pod defaults to be normalized: %+v", job.Spec.Template.Spec)
	}
}

func TestCloneStringMap(t *testing.T) {
	got := cloneStringMap(nil)
	if len(got) != 0 {
		t.Fatalf("expected empty map, got %+v", got)
	}
	in := map[string]string{"a": "1"}
	out := cloneStringMap(in)
	out["a"] = "2"
	if in["a"] != "1" {
		t.Fatal("expected clone to avoid mutating input")
	}
}

func TestIsRetryablePortForwardError(t *testing.T) {
	if isRetryablePortForwardError(nil) {
		t.Fatal("expected nil error to be non-retryable")
	}
	if isRetryablePortForwardError(errors.New("address already in use")) {
		t.Fatal("expected address in use to be non-retryable")
	}
	if !isRetryablePortForwardError(errors.New("connection reset by peer")) {
		t.Fatal("expected transient error to be retryable")
	}
}

func TestPodProgressAndSummary(t *testing.T) {
	now := metav1.NewTime(time.Unix(10, 0))
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace:         "ns",
			Name:              "pod",
			CreationTimestamp: now,
			Labels:            map[string]string{"app": "demo"},
			Annotations:       map[string]string{"a": "b"},
			DeletionTimestamp: &now,
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodPending,
			Reason: "",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "a", Ready: true, RestartCount: 1},
				{Name: "b", Ready: false, RestartCount: 2, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff"}}},
			},
		},
	}

	progress := podProgress(pod)
	if progress.ReadyContainers != 1 || progress.TotalContainers != 2 || progress.Reason != "ImagePullBackOff" {
		t.Fatalf("unexpected progress %+v", progress)
	}
	summary := podSummaryFromPod(pod)
	if summary.Phase != "Terminating" || !summary.Deleting || summary.Ready != "1/2" || summary.Restarts != 3 || summary.Reason != "ImagePullBackOff" {
		t.Fatalf("unexpected summary %+v", summary)
	}
}

func TestPreferredContainerFromExecErr(t *testing.T) {
	if got := preferredContainerFromExecErr(nil); got != "" {
		t.Fatalf("expected empty container for nil err, got %q", got)
	}
	err := errors.New("choose one of: [okdev-sidecar dev]")
	if got := preferredContainerFromExecErr(err); got != "dev" {
		t.Fatalf("expected dev preference, got %q", got)
	}
	err = errors.New("choose one of: [okdev-sidecar worker]")
	if got := preferredContainerFromExecErr(err); got != "worker" {
		t.Fatalf("expected non-sidecar fallback, got %q", got)
	}
	err = errors.New("choose one of: [okdev-sidecar]")
	if got := preferredContainerFromExecErr(err); got != "okdev-sidecar" {
		t.Fatalf("expected sole sidecar name, got %q", got)
	}
}
