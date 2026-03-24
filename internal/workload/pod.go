package workload

import (
	"context"
	"time"

	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
)

const DefaultTargetContainer = "dev"

type PodRuntime struct {
	SessionName        string
	Labels             map[string]string
	Annotations        map[string]string
	PodSpec            corev1.PodSpec
	Volumes            []corev1.Volume
	WorkspaceMountPath string
	SidecarImage       string
	Tmux               bool
	PreStop            string
	TargetContainer    string
}

func NewPodRuntime(sessionName string, labels, annotations map[string]string, podSpec corev1.PodSpec, volumes []corev1.Volume, workspaceMountPath, sidecarImage string, tmux bool, preStop, targetContainer string) *PodRuntime {
	return &PodRuntime{
		SessionName:        sessionName,
		Labels:             labels,
		Annotations:        annotations,
		PodSpec:            podSpec,
		Volumes:            volumes,
		WorkspaceMountPath: workspaceMountPath,
		SidecarImage:       sidecarImage,
		Tmux:               tmux,
		PreStop:            preStop,
		TargetContainer:    targetContainer,
	}
}

func (r *PodRuntime) Kind() string {
	return TypePod
}

func (r *PodRuntime) WorkloadName() string {
	return "okdev-" + r.SessionName
}

func (r *PodRuntime) Apply(ctx context.Context, k ApplyClient, namespace string) error {
	prepared, err := kube.PreparePodSpecForTarget(r.PodSpec, r.Volumes, r.WorkspaceMountPath, r.SidecarImage, r.Tmux, r.PreStop, r.effectiveTargetContainer())
	if err != nil {
		return err
	}
	name := r.WorkloadName()
	manifest, err := kube.BuildPodManifest(namespace, name, LabelsWithWorkload(r.Labels, name, "v1", "Pod"), r.Annotations, prepared)
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
		Container: r.effectiveTargetContainer(),
	}, nil
}

func (r *PodRuntime) effectiveTargetContainer() string {
	if r.TargetContainer != "" {
		return r.TargetContainer
	}
	return DefaultTargetContainer
}
