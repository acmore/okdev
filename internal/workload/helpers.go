package workload

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
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

// waitForCandidatePodReady polls for a candidate pod and waits for it to
// become ready. Used by multi-pod runtimes where pod discovery may take
// time after the workload is applied.
func waitForCandidatePodReady(
	ctx context.Context,
	k WaitClient,
	namespace string,
	selectCandidate candidateSelector,
	timeout time.Duration,
	onProgress func(kube.PodReadinessProgress),
	timeoutMessage string,
) error {
	deadline := time.Now().Add(timeout)
	var lastProgress kube.PodReadinessProgress
	haveProgress := false
	for {
		target, pods, err := selectCandidate(ctx, k, namespace)
		if err == nil && strings.TrimSpace(target.PodName) != "" {
			progress := summarizePodsAsProgress(pods)
			if onProgress != nil && (!haveProgress || progress != lastProgress) {
				onProgress(progress)
				lastProgress = progress
				haveProgress = true
			}
			waitTimeout := time.Until(deadline)
			if waitTimeout <= 0 {
				break
			}
			waitErr := k.WaitReadyWithProgress(ctx, namespace, target.PodName, waitTimeout, onProgress)
			if waitErr == nil {
				return nil
			}
			if shouldRetryCandidateWait(waitErr) {
				continue
			}
			return waitErr
		}
		if time.Now().After(deadline) {
			if err != nil {
				return err
			}
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
	return fmt.Errorf("%s", timeoutMessage)
}

func shouldRetryCandidateWait(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(strings.TrimSpace(err.Error()))
	return strings.Contains(msg, "was deleted while waiting for readiness") ||
		strings.Contains(msg, "is terminating") ||
		strings.Contains(msg, "not found")
}
