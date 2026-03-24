package workload

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

type JobRuntime struct {
	ManifestPath       string
	WorkspaceMountPath string
	SidecarImage       string
	Tmux               bool
	PreStop            string
	TargetContainer    string
	Volumes            []corev1.Volume
	Labels             map[string]string
	Annotations        map[string]string
}

type podLister interface {
	ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]kube.PodSummary, error)
}

func (r *JobRuntime) Kind() string {
	return TypeJob
}

func (r *JobRuntime) WorkloadName() string {
	job, err := r.load()
	if err != nil {
		return ""
	}
	return job.Name
}

func (r *JobRuntime) Apply(ctx context.Context, k ApplyClient, namespace string) error {
	job, err := r.load()
	if err != nil {
		return err
	}
	job.Namespace = namespace
	workloadLabels := LabelsWithWorkload(r.Labels, job.Name, "batch/v1", "Job")
	job.Labels = mergeStringMaps(job.Labels, workloadLabels)
	job.Annotations = mergeStringMaps(job.Annotations, r.Annotations)
	job.Spec.Template.Labels = mergeStringMaps(job.Spec.Template.Labels, workloadLabels)
	prepared, err := kube.PreparePodSpecForTarget(job.Spec.Template.Spec, r.Volumes, r.WorkspaceMountPath, r.SidecarImage, r.Tmux, r.PreStop, r.interactiveContainer())
	if err != nil {
		return err
	}
	job.Spec.Template.Spec = prepared
	manifest, err := kube.BuildJobManifest(namespace, *job)
	if err != nil {
		return err
	}
	return k.Apply(ctx, namespace, manifest)
}

func (r *JobRuntime) Delete(ctx context.Context, k DeleteClient, namespace string, ignoreNotFound bool) error {
	name := r.WorkloadName()
	if strings.TrimSpace(name) == "" {
		return fmt.Errorf("resolve job manifest name")
	}
	return k.Delete(ctx, namespace, "job", name, ignoreNotFound)
}

func (r *JobRuntime) WaitReady(ctx context.Context, k WaitClient, namespace string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	deadline := time.Now().Add(timeout)
	var lastProgress kube.PodReadinessProgress
	haveProgress := false
	for {
		target, pods, err := r.selectCandidate(ctx, k, namespace)
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
			return k.WaitReadyWithProgress(ctx, namespace, target.PodName, waitTimeout, onProgress)
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
	return fmt.Errorf("wait for job target pod readiness timed out")
}

func (r *JobRuntime) SelectTarget(ctx context.Context, k TargetClient, namespace string) (TargetRef, error) {
	target, _, err := r.selectCandidate(ctx, k, namespace)
	if err != nil {
		return TargetRef{}, err
	}
	target.Container = r.interactiveContainer()
	return target, nil
}

func (r *JobRuntime) selectCandidate(ctx context.Context, k podLister, namespace string) (TargetRef, []kube.PodSummary, error) {
	selector := buildLabelSelector(r.Labels)
	pods, err := k.ListPods(ctx, namespace, false, selector)
	if err != nil {
		return TargetRef{}, nil, err
	}
	if len(pods) == 0 {
		return TargetRef{}, nil, fmt.Errorf("no workload pods found for label selector %q", selector)
	}
	sort.Slice(pods, func(i, j int) bool {
		return comparePodPriority(pods[i], pods[j])
	})
	return TargetRef{PodName: pods[0].Name, Container: r.interactiveContainer()}, pods, nil
}

func (r *JobRuntime) interactiveContainer() string {
	if strings.TrimSpace(r.TargetContainer) != "" {
		return strings.TrimSpace(r.TargetContainer)
	}
	return DefaultTargetContainer
}

func (r *JobRuntime) load() (*batchv1.Job, error) {
	raw, err := os.ReadFile(r.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read job manifest %q: %w", r.ManifestPath, err)
	}
	var job batchv1.Job
	if err := yaml.Unmarshal(raw, &job); err != nil {
		return nil, fmt.Errorf("parse job manifest %q: %w", r.ManifestPath, err)
	}
	if strings.TrimSpace(job.Name) == "" {
		return nil, fmt.Errorf("job manifest %q is missing metadata.name", r.ManifestPath)
	}
	return &job, nil
}

func ResolveManifestPath(configPath, manifestPath string) string {
	if filepath.IsAbs(manifestPath) {
		return manifestPath
	}
	base := filepath.Dir(configPath)
	return filepath.Clean(filepath.Join(base, manifestPath))
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

func comparePodPriority(a, b kube.PodSummary) bool {
	score := func(p kube.PodSummary) int {
		score := 0
		if strings.EqualFold(p.Phase, string(corev1.PodRunning)) {
			score += 4
		}
		if strings.HasPrefix(strings.TrimSpace(p.Ready), "1/") || strings.EqualFold(strings.TrimSpace(p.Ready), "ready") {
			score += 2
		}
		return score
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
		if strings.HasPrefix(strings.TrimSpace(p.Ready), "1/") || strings.EqualFold(strings.TrimSpace(p.Ready), "ready") {
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
