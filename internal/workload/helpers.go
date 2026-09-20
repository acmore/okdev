package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	utilnet "k8s.io/apimachinery/pkg/util/net"
)

type podLister interface {
	ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]kube.PodSummary, error)
}

func ResolveManifestPath(configPath, manifestPath string) string {
	return config.ResolveWorkloadManifestPath(configPath, manifestPath)
}

func mergeStringMaps(base map[string]string, extra map[string]string) map[string]string {
	if len(base) == 0 && len(extra) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(base)+len(extra))
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

func buildLabelSelector(labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, labels[k]))
	}
	return strings.Join(parts, ",")
}

// DiscoveryLabelSelector returns a label selector using only the
// session-identifying labels needed for pod discovery. This avoids
// passing workload-specific labels that controllers might not
// propagate to child pods.
func DiscoveryLabelSelector(labels map[string]string) string {
	discovery := map[string]string{}
	if v, ok := labels["okdev.io/managed"]; ok {
		discovery["okdev.io/managed"] = v
	}
	if v, ok := labels["okdev.io/session"]; ok {
		discovery["okdev.io/session"] = v
	}
	if v, ok := labels["okdev.io/run-id"]; ok {
		discovery["okdev.io/run-id"] = v
	}
	if v, ok := labels["okdev.io/name"]; ok {
		discovery["okdev.io/name"] = v
	}
	if v, ok := labels["okdev.io/workload-type"]; ok {
		discovery["okdev.io/workload-type"] = v
	}
	return buildLabelSelector(discovery)
}

// ComparePodPriority is the canonical pod scoring function used by both
// runtime target selection and session view display. Pods are scored by:
// +8 for having a last-attach annotation, +4 for Running phase, +2 for
// Ready status. Ties broken by creation time (newest first), then name.
func ComparePodPriority(a, b kube.PodSummary) bool {
	score := func(p kube.PodSummary) int {
		s := 0
		if p.Deleting {
			s -= 16
		}
		if strings.TrimSpace(p.Annotations["okdev.io/last-attach"]) != "" {
			s += 8
		}
		if strings.EqualFold(p.Phase, string(corev1.PodRunning)) {
			s += 4
		}
		if isReadyString(strings.TrimSpace(p.Ready)) {
			s += 2
		}
		return s
	}
	as, bs := score(a), score(b)
	if as != bs {
		return as > bs
	}
	// Declared order beats creation time. Without this the target of a
	// multi-replica workload came down to which pod the controller created
	// last, so a PyTorchJob could land on a worker — and since the target is
	// also the sync hub, the workspace would re-bootstrap from a different pod
	// on the next run. An explicit attach still wins: it is scored above.
	if ar, br := podRank(a), podRank(b); ar != br {
		return ar < br
	}
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.After(b.CreatedAt)
	}
	return a.Name < b.Name
}

func summarizePodsAsProgress(pods []kube.PodSummary) kube.PodReadinessProgress {
	progress := kube.PodReadinessProgress{Reason: "waiting for candidate pods"}
	if len(pods) == 0 {
		return progress
	}
	progress.TotalContainers = len(pods)
	for _, p := range pods {
		if strings.EqualFold(p.Phase, string(corev1.PodRunning)) {
			progress.Phase = corev1.PodRunning
		}
		if isReadyString(strings.TrimSpace(p.Ready)) {
			progress.ReadyContainers++
		}
		if strings.TrimSpace(p.Reason) != "" {
			progress.Reason = p.Reason
		}
	}
	if progress.Phase == "" {
		progress.Phase = corev1.PodPending
	}
	return progress
}

// isReadyString returns true when a pod readiness string like "2/2" indicates
// all containers are ready, or when it equals "ready" (case-insensitive).
func isReadyString(s string) bool {
	if strings.EqualFold(s, "ready") {
		return true
	}
	num, denom, ok := strings.Cut(s, "/")
	return ok && strings.TrimSpace(num) == strings.TrimSpace(denom) && strings.TrimSpace(num) != "" && strings.TrimSpace(num) != "0"
}

type candidateSelector func(ctx context.Context, k podLister, namespace string) (TargetRef, []kube.PodSummary, error)

type FailedReadinessError struct {
	Pod    string
	Reason string
}

func (e *FailedReadinessError) Error() string {
	pod := strings.TrimSpace(e.Pod)
	reason := strings.TrimSpace(e.Reason)
	if reason == "" || reason == "-" {
		return fmt.Sprintf("pod %q failed while waiting for workload readiness", pod)
	}
	return fmt.Sprintf("pod %q failed while waiting for workload readiness: %s", pod, reason)
}

func IsFailedReadinessError(err error) bool {
	var failed *FailedReadinessError
	return errors.As(err, &failed)
}

// waitForCandidatePodReady polls for a candidate pod and waits for it to
// become ready, then waits for all remaining pods to become ready as well.
// Used by multi-pod runtimes where pod discovery may take time after the
// workload is applied.
func waitForCandidatePodReady(
	ctx context.Context,
	k WaitClient,
	namespace string,
	selectCandidate candidateSelector,
	timeout time.Duration,
	onProgress func(kube.PodReadinessProgress),
	failFastOnPodFailure bool,
	timeoutMessage string,
) (result error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	defer func() {
		if errors.Is(result, context.DeadlineExceeded) {
			result = fmt.Errorf("%s: %w", timeoutMessage, result)
		}
	}()
	deadline, _ := ctx.Deadline()
	var lastProgress kube.PodReadinessProgress
	haveProgress := false
	var backoff time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		target, pods, err := selectCandidate(ctx, k, namespace)
		if err != nil {
			if err := waitAfterSelectionError(ctx, err, &backoff, onProgress); err != nil {
				return err
			}
			continue
		}
		if strings.TrimSpace(target.PodName) == "" {
			if err := pauseReadiness(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		progress := summarizePodsAsProgress(pods)
		if onProgress != nil && (!haveProgress || progress != lastProgress) {
			onProgress(progress)
			lastProgress, haveProgress = progress, true
		}
		if err := failedPodError(pods, failFastOnPodFailure); err != nil {
			return err
		}
		waitErr := k.WaitReadyWithProgress(ctx, namespace, target.PodName, time.Until(deadline), onProgress)
		if waitErr == nil {
			if err := ctx.Err(); err != nil {
				return err
			}
			return waitForRemainingPods(ctx, k, namespace, selectCandidate, deadline, onProgress, failFastOnPodFailure)
		}
		if failFastOnPodFailure && failedReadinessWaitError(waitErr) {
			return &FailedReadinessError{Pod: target.PodName}
		}
		if !shouldRetryCandidateWait(waitErr) {
			return waitErr
		}
		if err := retryReadiness(ctx, &backoff, onProgress); err != nil {
			return err
		}
	}
}

// Recheck the full candidate set after each completed or interrupted watch.
func waitForRemainingPods(
	ctx context.Context,
	k WaitClient,
	namespace string,
	selectCandidate candidateSelector,
	deadline time.Time,
	onProgress func(kube.PodReadinessProgress),
	failFastOnPodFailure bool,
) error {
	ctx, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var backoff time.Duration
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		_, pods, err := selectCandidate(ctx, k, namespace)
		if err != nil {
			if err := waitAfterSelectionError(ctx, err, &backoff, onProgress); err != nil {
				return err
			}
			continue
		}
		if len(pods) == 0 {
			if err := pauseReadiness(ctx, 500*time.Millisecond); err != nil {
				return err
			}
			continue
		}
		if err := failedPodError(pods, failFastOnPodFailure); err != nil {
			return err
		}
		var pending []kube.PodSummary
		for _, p := range pods {
			if !failFastOnPodFailure && strings.EqualFold(strings.TrimSpace(p.Phase), string(corev1.PodFailed)) {
				continue
			}
			if !isReadyString(strings.TrimSpace(p.Ready)) {
				pending = append(pending, p)
			}
		}
		if len(pending) == 0 {
			return ctx.Err()
		}
		if onProgress != nil {
			progress := summarizePodsAsProgress(pods)
			progress.Reason = fmt.Sprintf("waiting for %d/%d pods", len(pending), len(pods))
			onProgress(progress)
		}
		waitErr := k.WaitReadyWithProgress(ctx, namespace, pending[0].Name, time.Until(deadline), onProgress)
		if waitErr == nil {
			backoff = 0
			continue
		}
		if failFastOnPodFailure && failedReadinessWaitError(waitErr) {
			return &FailedReadinessError{Pod: pending[0].Name}
		}
		if !shouldRetryCandidateWait(waitErr) {
			return waitErr
		}
		if err := retryReadiness(ctx, &backoff, onProgress); err != nil {
			return err
		}
	}
}

type candidateUnavailableError string

func (e candidateUnavailableError) Error() string { return string(e) }

func waitAfterSelectionError(ctx context.Context, err error, backoff *time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	var unavailable candidateUnavailableError
	if errors.As(err, &unavailable) {
		return pauseReadiness(ctx, 500*time.Millisecond)
	}
	if !shouldRetryCandidateWait(err) {
		return err
	}
	return retryReadiness(ctx, backoff, onProgress)
}

func pauseReadiness(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return ctx.Err()
	}
}

func retryReadiness(ctx context.Context, backoff *time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if *backoff == 0 {
		*backoff = 100 * time.Millisecond
	} else {
		*backoff = min(*backoff*2, 2*time.Second)
	}
	if onProgress != nil {
		onProgress(kube.PodReadinessProgress{Phase: corev1.PodUnknown, Reason: fmt.Sprintf("retrying readiness after interruption in %s", *backoff)})
	}
	return pauseReadiness(ctx, *backoff)
}

func shouldRetryCandidateWait(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
		return false
	}
	if apierrors.IsNotFound(err) || apierrors.IsResourceExpired(err) || apierrors.IsGone(err) || apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) || apierrors.IsTooManyRequests(err) || apierrors.IsServiceUnavailable(err) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) || errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.ECONNREFUSED) || errors.Is(err, syscall.EPIPE) || utilnet.IsProbableEOF(err) {
		return true
	}
	var networkError net.Error
	if errors.As(err, &networkError) && (networkError.Timeout() || networkError.Temporary()) {
		return true
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "http2: client connection lost") ||
		strings.Contains(msg, "was deleted while waiting for readiness") ||
		strings.Contains(msg, "is terminating")
}

func failedReadinessWaitError(err error) bool {
	return errors.Is(err, kube.ErrPodFailedReadiness)
}

func failedPodError(pods []kube.PodSummary, enabled bool) error {
	if !enabled {
		return nil
	}
	for _, p := range pods {
		if !strings.EqualFold(strings.TrimSpace(p.Phase), string(corev1.PodFailed)) {
			continue
		}
		reason := strings.TrimSpace(p.Reason)
		return &FailedReadinessError{Pod: p.Name, Reason: reason}
	}
	return nil
}
