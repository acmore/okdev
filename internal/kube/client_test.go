package kube

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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

func TestNormalizePodSpecDefaultsPreservesNonDefaultValues(t *testing.T) {
	grace := int64(45)
	spec := corev1.PodSpec{
		TerminationGracePeriodSeconds: &grace,
		DNSPolicy:                     corev1.DNSDefault,
		SchedulerName:                 "custom-scheduler",
		Containers: []corev1.Container{{
			Name:                     "dev",
			ImagePullPolicy:          corev1.PullAlways,
			TerminationMessagePath:   "/tmp/custom",
			TerminationMessagePolicy: corev1.TerminationMessageFallbackToLogsOnError,
			Ports:                    []corev1.ContainerPort{{ContainerPort: 53, Protocol: corev1.ProtocolUDP}},
		}},
	}

	normalizePodSpecDefaults(&spec)

	if spec.TerminationGracePeriodSeconds == nil || *spec.TerminationGracePeriodSeconds != grace {
		t.Fatalf("expected custom termination grace period to be preserved: %+v", spec.TerminationGracePeriodSeconds)
	}
	if spec.DNSPolicy != corev1.DNSDefault {
		t.Fatalf("expected custom DNS policy to be preserved, got %q", spec.DNSPolicy)
	}
	if spec.SchedulerName != "custom-scheduler" {
		t.Fatalf("expected custom scheduler to be preserved, got %q", spec.SchedulerName)
	}
	container := spec.Containers[0]
	if container.ImagePullPolicy != corev1.PullAlways || container.TerminationMessagePath != "/tmp/custom" || container.TerminationMessagePolicy != corev1.TerminationMessageFallbackToLogsOnError {
		t.Fatalf("expected non-default container fields to be preserved: %+v", container)
	}
	if got := container.Ports[0].Protocol; got != corev1.ProtocolUDP {
		t.Fatalf("expected non-default protocol to be preserved, got %q", got)
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

func TestBuildPodLogOptions(t *testing.T) {
	tail := int64(100)
	got := buildPodLogOptions(LogStreamOptions{
		Container: " dev ",
		Follow:    true,
		Previous:  true,
		TailLines: &tail,
		Since:     5*time.Minute + 200*time.Millisecond,
	})

	if got.Container != "dev" {
		t.Fatalf("expected trimmed container, got %q", got.Container)
	}
	if !got.Follow || !got.Previous {
		t.Fatalf("expected follow and previous to be true: %+v", got)
	}
	if got.TailLines == nil || *got.TailLines != tail {
		t.Fatalf("unexpected tail lines: %+v", got.TailLines)
	}
	if got.SinceSeconds == nil || *got.SinceSeconds != 300 {
		t.Fatalf("unexpected since seconds: %+v", got.SinceSeconds)
	}
}

func TestBuildPodLogOptionsOmitsUnsetFields(t *testing.T) {
	got := buildPodLogOptions(LogStreamOptions{})
	if got.Container != "" {
		t.Fatalf("expected empty container, got %q", got.Container)
	}
	if got.Follow || got.Previous {
		t.Fatalf("expected follow/previous false, got %+v", got)
	}
	if got.TailLines != nil || got.SinceSeconds != nil {
		t.Fatalf("expected nil optional fields, got %+v", got)
	}
}

func TestTempDownloadPath(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "artifact.txt")

	tempPath, err := tempDownloadPath(localPath)
	if err != nil {
		t.Fatalf("tempDownloadPath: %v", err)
	}
	if tempPath == localPath {
		t.Fatal("expected temp path to differ from final path")
	}
	if filepath.Dir(tempPath) != dir {
		t.Fatalf("expected temp file in %q, got %q", dir, filepath.Dir(tempPath))
	}
	if _, err := os.Stat(tempPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected temp file to be removed after reservation, got err=%v", err)
	}
}

func TestBuildPodLogOptionsRoundsShortDurationsUpToOneSecond(t *testing.T) {
	got := buildPodLogOptions(LogStreamOptions{Since: 500 * time.Millisecond})
	if got.SinceSeconds == nil || *got.SinceSeconds != 1 {
		t.Fatalf("expected short duration to round up to one second, got %+v", got.SinceSeconds)
	}
}

func TestPodContainerNames(t *testing.T) {
	pod := &corev1.Pod{
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "dev"},
				{Name: " okdev-sidecar "},
				{Name: ""},
			},
		},
	}

	got := podContainerNames(pod)
	want := []string{"dev", "okdev-sidecar"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected container names: got %#v want %#v", got, want)
	}
}

func TestPreferredContainerFromExecErrReturnsEmptyWithoutChoices(t *testing.T) {
	if got := preferredContainerFromExecErr(errors.New("permission denied")); got != "" {
		t.Fatalf("expected empty preferred container, got %q", got)
	}
}

func TestPodSummaryFromPodPrefersPodReason(t *testing.T) {
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Namespace: "ns",
			Name:      "pod",
		},
		Status: corev1.PodStatus{
			Phase:  corev1.PodFailed,
			Reason: "Evicted",
			ContainerStatuses: []corev1.ContainerStatus{
				{Name: "dev", RestartCount: 2, State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "CrashLoopBackOff"}}},
			},
		},
	}
	summary := podSummaryFromPod(pod)
	if summary.Reason != "Evicted" {
		t.Fatalf("expected pod-level reason to win, got %+v", summary)
	}
	if summary.Restarts != 2 {
		t.Fatalf("expected restart count, got %+v", summary)
	}
}

func TestClientCacheKeyChangesWhenKubeconfigChanges(t *testing.T) {
	dir := t.TempDir()
	kubeconfig := filepath.Join(dir, "config")
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", kubeconfig)

	client := &Client{Context: "dev"}
	first, err := client.cacheKey()
	if err != nil {
		t.Fatal(err)
	}

	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(kubeconfig, []byte("apiVersion: v1\nkind: Config\nclusters: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	second, err := client.cacheKey()
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatalf("expected cache key to change after kubeconfig update: %q", first)
	}
}

func TestClientCacheKeyMarksMissingKubeconfig(t *testing.T) {
	path := filepath.Join(t.TempDir(), "missing-config")
	t.Setenv("KUBECONFIG", path)

	key, err := (&Client{}).cacheKey()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(key, path+":missing") {
		t.Fatalf("expected missing kubeconfig marker, got %q", key)
	}
}

func TestClientRestConfigUsesContextOverride(t *testing.T) {
	kubeconfig := writeTestKubeconfig(t)
	t.Setenv("KUBECONFIG", kubeconfig)

	cfg, err := (&Client{Context: "dev"}).restConfig()
	if err != nil {
		t.Fatalf("unexpected rest config error: %v", err)
	}
	if cfg.Host != "https://127.0.0.1:6443" {
		t.Fatalf("unexpected host %q", cfg.Host)
	}
	if cfg.Timeout != 0 {
		t.Fatalf("expected timeout to be disabled, got %s", cfg.Timeout)
	}
}

func TestClientClientsReusesCache(t *testing.T) {
	kubeconfig := writeTestKubeconfig(t)
	t.Setenv("KUBECONFIG", kubeconfig)

	client := &Client{Context: "dev"}
	cs1, dc1, mapper1, cfg1, err := client.clients()
	if err != nil {
		t.Fatalf("unexpected first clients error: %v", err)
	}
	cs2, dc2, mapper2, cfg2, err := client.clients()
	if err != nil {
		t.Fatalf("unexpected second clients error: %v", err)
	}
	if cs1 != cs2 || dc1 != dc2 || mapper1 != mapper2 || cfg1 != cfg2 {
		t.Fatal("expected cached client instances to be reused")
	}
}

func TestClientResourceExistsValidatesRequiredName(t *testing.T) {
	exists, err := (&Client{}).ResourceExists(t.Context(), "default", "v1", "Pod", " ")
	if err == nil {
		t.Fatal("expected validation error")
	}
	if exists {
		t.Fatal("expected false when validation fails")
	}
}

func TestClientGetResourceAnnotationValidatesRequiredName(t *testing.T) {
	_, _, err := (&Client{}).GetResourceAnnotation(t.Context(), "default", "v1", "Pod", " ", "okdev.io/last-applied-spec")
	if err == nil {
		t.Fatal("expected error for blank name")
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b"); got != `'a'\''b'` {
		t.Fatalf("unexpected quoted string: %s", got)
	}
}

func writeTestKubeconfig(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	content := `apiVersion: v1
kind: Config
clusters:
- cluster:
    server: https://127.0.0.1:6443
  name: dev
contexts:
- context:
    cluster: dev
    user: dev
  name: dev
current-context: dev
users:
- name: dev
  user:
    token: fake
`
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestIsPodEventRelevant(t *testing.T) {
	relevant := []string{"Pulling", "Pulled", "FailedScheduling", "BackOff", "Failed", "Unhealthy", "FailedMount", "FailedAttachVolume"}
	for _, reason := range relevant {
		if !isPodEventRelevant(reason) {
			t.Errorf("expected %q to be relevant", reason)
		}
	}
	irrelevant := []string{"Scheduled", "Created", "Started", "Normal", ""}
	for _, reason := range irrelevant {
		if isPodEventRelevant(reason) {
			t.Errorf("expected %q to be irrelevant", reason)
		}
	}
}
