package workload

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/yaml"
)

type GenericRuntime struct {
	WorkloadKind       string
	ManifestPath       string
	WorkspaceMountPath string
	SidecarImage       string
	Tmux               bool
	PreStop            string
	TargetContainer    string
	Volumes            []corev1.Volume
	Labels             map[string]string
	Annotations        map[string]string
	Inject             []config.WorkloadInjectSpec

	loadMu     sync.Mutex
	loadedBase *unstructured.Unstructured
}

func (r *GenericRuntime) Kind() string {
	if strings.TrimSpace(r.WorkloadKind) != "" {
		return strings.TrimSpace(r.WorkloadKind)
	}
	return TypeGeneric
}

func (r *GenericRuntime) WorkloadName() string {
	obj, err := r.load()
	if err != nil {
		slog.Warn("failed to resolve workload name from manifest", "path", r.ManifestPath, "error", err)
		return ""
	}
	return obj.GetName()
}

func (r *GenericRuntime) WorkloadRef() (string, string, string, error) {
	obj, err := r.load()
	if err != nil {
		return "", "", "", err
	}
	return obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), nil
}

func (r *GenericRuntime) Apply(ctx context.Context, k ApplyClient, namespace string) error {
	obj, err := r.load()
	if err != nil {
		return err
	}
	workloadLabels := LabelsWithWorkload(r.Labels, obj.GetName(), obj.GetKind())
	workloadAnnotations := AnnotationsWithWorkload(r.Annotations, obj.GetName(), obj.GetAPIVersion(), obj.GetKind())
	obj.SetLabels(mergeStringMaps(obj.GetLabels(), workloadLabels))
	obj.SetAnnotations(mergeStringMaps(obj.GetAnnotations(), workloadAnnotations))
	for _, inject := range r.Inject {
		templateMap, err := resolveMapPath(obj.Object, inject.Path)
		if err != nil {
			return err
		}
		template, err := decodePodTemplateSpec(templateMap)
		if err != nil {
			return fmt.Errorf("decode inject path %s: %w", inject.Path, err)
		}
		templateLabels := mergeStringMaps(template.Labels, workloadLabels)
		templateAnnotations := mergeStringMaps(template.Annotations, workloadAnnotations)
		templateLabels["okdev.io/attachable"] = boolLabel(injectAttachable(inject))
		if role := roleFromInjectPath(inject.Path); role != "" {
			templateLabels["okdev.io/workload-role"] = role
		}
		template.Labels = templateLabels
		template.Annotations = templateAnnotations
		if inject.Sidecar == nil || *inject.Sidecar {
			template.Spec, err = kube.PreparePodSpecForTarget(template.Spec, r.Volumes, r.WorkspaceMountPath, r.SidecarImage, r.Tmux, r.PreStop, r.interactiveContainer())
			if err != nil {
				return err
			}
		}
		updated, err := encodePodTemplateSpec(template)
		if err != nil {
			return err
		}
		if err := writeMapPath(obj.Object, inject.Path, updated); err != nil {
			return err
		}
	}
	manifest, err := yaml.Marshal(obj.Object)
	if err != nil {
		return fmt.Errorf("marshal generic manifest: %w", err)
	}
	return k.Apply(ctx, namespace, manifest)
}

func (r *GenericRuntime) Delete(ctx context.Context, k DeleteClient, namespace string, ignoreNotFound bool) error {
	obj, err := r.load()
	if err != nil {
		return err
	}
	return k.DeleteByRef(ctx, namespace, obj.GetAPIVersion(), obj.GetKind(), obj.GetName(), ignoreNotFound)
}

func (r *GenericRuntime) WaitReady(ctx context.Context, k WaitClient, namespace string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	return waitForCandidatePodReady(ctx, k, namespace, r.selectCandidate, timeout, onProgress,
		fmt.Sprintf("wait for %s workload target pod readiness timed out", r.Kind()))
}

func (r *GenericRuntime) SelectTarget(ctx context.Context, k TargetClient, namespace string) (TargetRef, error) {
	target, _, err := r.selectCandidate(ctx, k, namespace)
	if err != nil {
		return TargetRef{}, err
	}
	target.Container = r.interactiveContainer()
	return target, nil
}

func (r *GenericRuntime) selectCandidate(ctx context.Context, k podLister, namespace string) (TargetRef, []kube.PodSummary, error) {
	selector := DiscoveryLabelSelector(r.Labels)
	pods, err := k.ListPods(ctx, namespace, false, selector)
	if err != nil {
		return TargetRef{}, nil, err
	}
	if len(pods) == 0 {
		return TargetRef{}, nil, fmt.Errorf("no workload pods found for label selector %q", selector)
	}
	eligible := make([]kube.PodSummary, 0, len(pods))
	for _, pod := range pods {
		if strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/attachable"]), "true") {
			eligible = append(eligible, pod)
		}
	}
	if len(eligible) == 0 {
		return TargetRef{}, pods, fmt.Errorf("no attachable pods found for label selector %q", selector)
	}
	sort.Slice(eligible, func(i, j int) bool {
		return ComparePodPriority(eligible[i], eligible[j])
	})
	return TargetRef{
		PodName:   eligible[0].Name,
		Container: r.interactiveContainer(),
		Role:      strings.TrimSpace(eligible[0].Labels["okdev.io/workload-role"]),
	}, pods, nil
}

func (r *GenericRuntime) interactiveContainer() string {
	if strings.TrimSpace(r.TargetContainer) != "" {
		return strings.TrimSpace(r.TargetContainer)
	}
	return DefaultTargetContainer
}

func (r *GenericRuntime) load() (*unstructured.Unstructured, error) {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	if r.loadedBase != nil {
		return r.loadedBase.DeepCopy(), nil
	}
	raw, err := os.ReadFile(r.ManifestPath)
	if err != nil {
		return nil, fmt.Errorf("read generic manifest %q: %w", r.ManifestPath, err)
	}
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse generic manifest %q: %w", r.ManifestPath, err)
	}
	u := &unstructured.Unstructured{Object: obj}
	if strings.TrimSpace(u.GetAPIVersion()) == "" || strings.TrimSpace(u.GetKind()) == "" {
		return nil, fmt.Errorf("generic manifest %q is missing apiVersion/kind", r.ManifestPath)
	}
	if strings.TrimSpace(u.GetName()) == "" {
		return nil, fmt.Errorf("generic manifest %q is missing metadata.name", r.ManifestPath)
	}
	r.loadedBase = u.DeepCopy()
	return u.DeepCopy(), nil
}

// resolveMapPath descends into all path segments and returns the nested map
// at the final key. For "spec.template" it returns obj["spec"]["template"].
func resolveMapPath(root map[string]any, path string) (map[string]any, error) {
	current := root
	for _, part := range strings.Split(strings.TrimSpace(path), ".") {
		if strings.TrimSpace(part) == "" {
			continue
		}
		next, ok := current[part]
		if !ok {
			return nil, fmt.Errorf("resolve path %q: missing %q", path, part)
		}
		child, ok := next.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("resolve path %q: %q is not an object", path, part)
		}
		current = child
	}
	return current, nil
}

// writeMapPath descends to the parent of the final path segment and replaces
// the last key. For "spec.template" it sets obj["spec"]["template"] = value.
func writeMapPath(root map[string]any, path string, value map[string]any) error {
	parts := strings.Split(strings.TrimSpace(path), ".")
	if len(parts) == 0 {
		return fmt.Errorf("empty path")
	}
	current := root
	for _, part := range parts[:len(parts)-1] {
		next, ok := current[part]
		if !ok {
			return fmt.Errorf("resolve path %q: missing %q", path, part)
		}
		child, ok := next.(map[string]any)
		if !ok {
			return fmt.Errorf("resolve path %q: %q is not an object", path, part)
		}
		current = child
	}
	current[parts[len(parts)-1]] = value
	return nil
}

func decodePodTemplateSpec(src map[string]any) (corev1.PodTemplateSpec, error) {
	var template corev1.PodTemplateSpec
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(src, &template); err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	return template, nil
}

func encodePodTemplateSpec(template corev1.PodTemplateSpec) (map[string]any, error) {
	out, err := runtime.DefaultUnstructuredConverter.ToUnstructured(&template)
	if err != nil {
		return nil, err
	}
	return out, nil
}

func injectAttachable(inject config.WorkloadInjectSpec) bool {
	if inject.Attachable != nil {
		return *inject.Attachable
	}
	if inject.Sidecar != nil && !*inject.Sidecar {
		return false
	}
	return true
}

func boolLabel(v bool) string {
	if v {
		return "true"
	}
	return "false"
}

func roleFromInjectPath(path string) string {
	parts := strings.Split(strings.TrimSpace(path), ".")
	if len(parts) < 2 {
		return ""
	}
	for i := len(parts) - 1; i >= 0; i-- {
		part := strings.TrimSpace(parts[i])
		if part == "" || strings.EqualFold(part, "template") || strings.EqualFold(part, "spec") {
			continue
		}
		return part
	}
	return ""
}
