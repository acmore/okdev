package workload

import (
	"context"
	"time"

	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
)

const DefaultTargetContainer = "dev"

type PodRuntime struct {
	SessionName string
	Labels      map[string]string
	Annotations map[string]string
	PodSpec     corev1.PodSpec
}

func NewPodRuntime(sessionName string, labels, annotations map[string]string, podSpec corev1.PodSpec) *PodRuntime {
	return &PodRuntime{
		SessionName: sessionName,
		Labels:      labels,
		Annotations: annotations,
		PodSpec:     podSpec,
	}
}

func (r *PodRuntime) Kind() string {
	return TypePod
}

func (r *PodRuntime) WorkloadName() string {
	return "okdev-" + r.SessionName
}

func (r *PodRuntime) Apply(ctx context.Context, k ApplyClient, namespace string) error {
	manifest, err := kube.BuildPodManifest(namespace, r.WorkloadName(), LabelsWithWorkload(r.Labels, r.WorkloadName(), "v1", "Pod"), r.Annotations, r.PodSpec)
	if err != nil {
		return err
	}
	return k.Apply(ctx, namespace, manifest)
}

func (r *PodRuntime) Delete(ctx context.Context, k DeleteClient, namespace string, ignoreNotFound bool) error {
	return k.Delete(ctx, namespace, "pod", r.WorkloadName(), ignoreNotFound)
}

func (r *PodRuntime) WaitReady(ctx context.Context, k WaitClient, namespace string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	return k.WaitReadyWithProgress(ctx, namespace, r.WorkloadName(), timeout, onProgress)
}

func (r *PodRuntime) SelectTarget(ctx context.Context, k TargetClient, namespace string) (TargetRef, error) {
	if _, err := k.GetPodSummary(ctx, namespace, r.WorkloadName()); err != nil {
		return TargetRef{}, err
	}
	return TargetRef{
		PodName:   r.WorkloadName(),
		Container: DefaultTargetContainer,
	}, nil
}
