package cli

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
)

type sessionPodLister interface {
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
}

func sessionRuntime(cfg *config.DevEnvironment, cfgPath, sessionName, workloadName string, labels, annotations map[string]string, tmux bool, preStop string) (workload.Runtime, error) {
	targetContainer := resolveTargetContainer(cfg)
	workspaceMountPath := cfg.EffectiveWorkspaceMountPath(cfgPath)
	manifestPath := strings.TrimSpace(cfg.Spec.Workload.ManifestPath)
	manifestResolvedPath := workload.ResolveManifestPath(cfgPath, manifestPath)

	snap := config.BuildWorkloadSnapshot(cfg, workspaceMountPath, targetContainer, tmux, cfg.Spec.SSH.Shell, preStop, manifestPath, manifestResolvedPath)
	snapJSON, _ := snap.JSON()
	snapHash, _ := snap.SHA256()

	workloadType := strings.TrimSpace(cfg.Spec.Workload.Type)
	if workloadType == "" {
		workloadType = workload.TypePod
	}
	switch workloadType {
	case workload.TypePod, workload.TypeJob, workload.TypeGeneric, workload.TypePyTorchJob:
	default:
		return nil, fmt.Errorf("unsupported workload type %q", cfg.Spec.Workload.Type)
	}

	// A session with no local state and no live pod to discover has no recorded
	// workload name, and `{{ .WorkloadName }}` would render empty — a manifest
	// with no metadata.name. The synthesized-pod path used to cover this for
	// pod; the same fallback now serves every type.
	if strings.TrimSpace(workloadName) == "" {
		workloadName = podName(sessionName)
	}

	rt := &workload.GenericRuntime{
		SessionName:          sessionName,
		WorkloadNameOverride: workloadName,
		WorkloadKind:         workloadType,
		ManifestPath:         manifestResolvedPath,
		WorkspaceMountPath:   workspaceMountPath,
		SidecarImage:         cfg.Spec.Sidecar.Image,
		SidecarResources:     cfg.Spec.Sidecar.Resources,
		Tmux:                 tmux,
		Shell:                cfg.Spec.SSH.Shell,
		PreStop:              preStop,
		TargetContainer:      targetContainer,
		SyncRemoteRoots:      cfg.AdditionalSyncRemoteRoots(),
		Labels:               labels,
		Annotations:          annotations,
		Inject:               cfg.EffectiveWorkloadInject(),
		LastAppliedSpecJSON:  snapJSON,
		LastAppliedSpecHash:  snapHash,
	}
	return rt, nil
}

func sessionRuntimeForExisting(ctx context.Context, cfg *config.DevEnvironment, cfgPath, namespace, sessionName string, pods sessionPodLister) (workload.Runtime, error) {
	info, _ := session.LoadInfo(sessionName)
	runID := strings.TrimSpace(info.RunID)
	workloadName := strings.TrimSpace(info.WorkloadName)
	if pods != nil {
		discoveredRunID, discoveredWorkloadName, err := discoverRunIdentityFromPods(ctx, pods, namespace, sessionName)
		if err != nil {
			slog.Debug("failed to discover run identity from live pods", "session", sessionName, "error", err)
		} else {
			if discoveredRunID != "" {
				runID = discoveredRunID
			}
			if discoveredWorkloadName != "" {
				workloadName = discoveredWorkloadName
			}
		}
	}
	return sessionRuntime(cfg, cfgPath, sessionName, workloadName, discoveryLabelsForSession(cfg, sessionName, runID), nil, cfg.Spec.SSH.PersistentSessionEnabled(), resolvePreStopCommand(cfg, cfgPath))
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

type targetResolverClient interface {
	kubeClientTargetGetter
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
}

func resolveTargetRef(ctx context.Context, opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, k targetResolverClient) (workload.TargetRef, error) {
	target, err := loadTargetRef(sessionName)
	if err != nil {
		return workload.TargetRef{}, err
	}
	if err := validatePinnedTarget(ctx, k, namespace, target); err == nil {
		return target, nil
	}

	cfgPath, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return workload.TargetRef{}, err
	}
	runtime, err := sessionRuntimeForExisting(ctx, cfg, cfgPath, namespace, sessionName, k)
	if err != nil {
		return workload.TargetRef{}, err
	}
	target, err = runtime.SelectTarget(ctx, k, namespace)
	if err != nil {
		return workload.TargetRef{}, err
	}
	if err := persistTargetRef(sessionName, target); err != nil {
		return workload.TargetRef{}, err
	}
	return target, nil
}

func refreshTargetRef(ctx context.Context, opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, k targetResolverClient, current workload.TargetRef) (workload.TargetRef, error) {
	if strings.TrimSpace(current.PodName) != "" {
		if err := validatePinnedTarget(ctx, k, namespace, current); err == nil {
			return current, nil
		}
	}
	return resolveTargetRef(ctx, opts, cfg, namespace, sessionName, k)
}

func resolveFreshTargetRef(ctx context.Context, opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, k targetResolverClient) (workload.TargetRef, error) {
	cfgPath, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return workload.TargetRef{}, err
	}
	runtime, err := sessionRuntimeForExisting(ctx, cfg, cfgPath, namespace, sessionName, k)
	if err != nil {
		return workload.TargetRef{}, err
	}
	target, err := runtime.SelectTarget(ctx, k, namespace)
	if err != nil {
		return workload.TargetRef{}, err
	}
	if err := persistTargetRef(sessionName, target); err != nil {
		return workload.TargetRef{}, err
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
	checkCtx, cancel := context.WithTimeout(ctx, targetValidationTimeout)
	defer cancel()
	summary, err := k.GetPodSummary(checkCtx, namespace, target.PodName)
	if err != nil {
		return fmt.Errorf("pinned target %s is not available: %w", target.PodName, err)
	}
	if summary.Deleting {
		return fmt.Errorf("pinned target %s is terminating", target.PodName)
	}
	return nil
}

func resolveTargetContainer(cfg *config.DevEnvironment) string {
	if strings.TrimSpace(cfg.Spec.Workload.Attach.Container) != "" {
		return strings.TrimSpace(cfg.Spec.Workload.Attach.Container)
	}
	return workload.DefaultTargetContainer
}

func discoveryLabelsForSession(cfg *config.DevEnvironment, sessionName, runID string) map[string]string {
	labels := map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/session": sessionName,
		"okdev.io/name":    cfg.Metadata.Name,
	}
	if runID = strings.TrimSpace(runID); runID != "" {
		labels["okdev.io/run-id"] = runID
	}
	if workloadType := strings.TrimSpace(cfg.Spec.Workload.Type); workloadType != "" {
		labels["okdev.io/workload-type"] = workloadType
	}
	// Two profiles can share a type (a one-GPU "dev" pod and an eight-GPU
	// "big" pod), so only the name can tell a workload switch from a no-op.
	if profile := strings.TrimSpace(cfg.SelectedWorkload()); profile != "" {
		labels["okdev.io/workload-profile"] = profile
	}
	return labels
}

func discoverRunIdentityFromPods(ctx context.Context, pods sessionPodLister, namespace, sessionName string) (string, string, error) {
	selector := "okdev.io/managed=true,okdev.io/session=" + sessionName
	summaries, err := pods.ListPods(ctx, namespace, false, selector)
	if err != nil {
		return "", "", fmt.Errorf("discover live session pods: %w", err)
	}
	candidates := make([]kube.PodSummary, 0, len(summaries))
	for _, pod := range summaries {
		if pod.Deleting {
			continue
		}
		if strings.TrimSpace(pod.Labels["okdev.io/run-id"]) == "" {
			continue
		}
		if workloadNameFromPodSummary(pod) == "" {
			continue
		}
		candidates = append(candidates, pod)
	}
	if len(candidates) == 0 {
		return "", "", nil
	}
	sort.Slice(candidates, func(i, j int) bool {
		return workload.ComparePodPriority(candidates[i], candidates[j])
	})
	return strings.TrimSpace(candidates[0].Labels["okdev.io/run-id"]), workloadNameFromPodSummary(candidates[0]), nil
}

func workloadNameFromPodSummary(pod kube.PodSummary) string {
	if name := strings.TrimSpace(pod.Labels["okdev.io/workload-name"]); name != "" {
		return name
	}
	return strings.TrimSpace(pod.Annotations["okdev.io/workload-name"])
}
