package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	corev1 "k8s.io/api/core/v1"
)

func sessionRuntime(cfg *config.DevEnvironment, cfgPath, sessionName string, labels, annotations map[string]string, podSpec corev1.PodSpec, volumes []corev1.Volume, tmux bool, preStop string) (workload.Runtime, error) {
	switch strings.TrimSpace(cfg.Spec.Workload.Type) {
	case "", workload.TypePod:
		return workload.NewPodRuntime(sessionName, labels, annotations, podSpec), nil
	case workload.TypeJob:
		return &workload.JobRuntime{
			ManifestPath:       workload.ResolveManifestPath(cfgPath, cfg.Spec.Workload.ManifestPath),
			WorkspaceMountPath: cfg.WorkspaceMountPath(),
			SidecarImage:       cfg.Spec.Sidecar.Image,
			Tmux:               tmux,
			PreStop:            preStop,
			TargetContainer:    cfg.Spec.Workload.Attach.Container,
			Volumes:            volumes,
			Labels:             labels,
			Annotations:        annotations,
		}, nil
	case workload.TypeGeneric:
		return &workload.GenericRuntime{
			WorkloadKind:       workload.TypeGeneric,
			ManifestPath:       workload.ResolveManifestPath(cfgPath, cfg.Spec.Workload.ManifestPath),
			WorkspaceMountPath: cfg.WorkspaceMountPath(),
			SidecarImage:       cfg.Spec.Sidecar.Image,
			Tmux:               tmux,
			PreStop:            preStop,
			TargetContainer:    cfg.Spec.Workload.Attach.Container,
			Volumes:            volumes,
			Labels:             labels,
			Annotations:        annotations,
			Inject:             cfg.Spec.Workload.Inject,
		}, nil
	case workload.TypePyTorchJob:
		return &workload.GenericRuntime{
			WorkloadKind:       workload.TypePyTorchJob,
			ManifestPath:       workload.ResolveManifestPath(cfgPath, cfg.Spec.Workload.ManifestPath),
			WorkspaceMountPath: cfg.WorkspaceMountPath(),
			SidecarImage:       cfg.Spec.Sidecar.Image,
			Tmux:               tmux,
			PreStop:            preStop,
			TargetContainer:    cfg.Spec.Workload.Attach.Container,
			Volumes:            volumes,
			Labels:             labels,
			Annotations:        annotations,
			Inject:             cfg.Spec.Workload.Inject,
		}, nil
	default:
		return nil, fmt.Errorf("unsupported workload type %q", cfg.Spec.Workload.Type)
	}
}

func sessionRuntimeForExisting(cfg *config.DevEnvironment, cfgPath, sessionName string) (workload.Runtime, error) {
	return sessionRuntime(cfg, cfgPath, sessionName, nil, nil, corev1.PodSpec{}, cfg.EffectiveVolumes(), cfg.Spec.SSH.PersistentSessionEnabled(), resolvePreStopCommand(cfg, cfgPath))
}

func defaultTargetRef(sessionName string) workload.TargetRef {
	return workload.TargetRef{
		PodName:   podName(sessionName),
		Container: workload.DefaultTargetContainer,
	}
}

func loadTargetRef(sessionName string) (workload.TargetRef, error) {
	target, err := session.LoadTarget(sessionName)
	if err != nil {
		return workload.TargetRef{}, err
	}
	if strings.TrimSpace(target.PodName) == "" {
		return defaultTargetRef(sessionName), nil
	}
	if strings.TrimSpace(target.Container) == "" {
		target.Container = workload.DefaultTargetContainer
	}
	return target, nil
}

func persistTargetRef(sessionName string, target workload.TargetRef) error {
	if strings.TrimSpace(target.PodName) == "" {
		target = defaultTargetRef(sessionName)
	}
	if strings.TrimSpace(target.Container) == "" {
		target.Container = workload.DefaultTargetContainer
	}
	return session.SaveTarget(sessionName, target)
}

type kubeClientTargetGetter interface {
	GetPodSummary(context.Context, string, string) (*kube.PodSummary, error)
}

func validatePinnedTarget(ctx context.Context, k kubeClientTargetGetter, namespace string, target workload.TargetRef) error {
	checkCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if _, err := k.GetPodSummary(checkCtx, namespace, target.PodName); err != nil {
		return fmt.Errorf("pinned target %s is not available: %w", target.PodName, err)
	}
	return nil
}
