package workload

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"
	"text/template"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/yaml"
)

const DefaultTargetContainer = "dev"

type GenericRuntime struct {
	SessionName          string
	WorkloadNameOverride string
	WorkloadKind         string
	ManifestPath         string
	// ManifestBytes, when set, is the manifest source instead of ManifestPath.
	// Bytes-sourced manifests are synthesized by okdev rather than authored by
	// the user, so they are applied verbatim — no Go-template rendering, which
	// would reject user pod specs that legitimately contain "{{".
	ManifestBytes       []byte
	WorkspaceMountPath  string
	SidecarImage        string
	SidecarResources    corev1.ResourceRequirements
	Tmux                bool
	Shell               string
	PreStop             string
	TargetContainer     string
	Volumes             []corev1.Volume
	SyncRemoteRoots     []string
	Labels              map[string]string
	Annotations         map[string]string
	Inject              []config.WorkloadInjectSpec
	LastAppliedSpecJSON string
	LastAppliedSpecHash string

	loadMu     sync.Mutex
	loadedBase *unstructured.Unstructured
	loadedFrom manifestCacheStamp
}

type manifestCacheStamp struct {
	path    string
	modTime time.Time
	size    int64
}

func (r *GenericRuntime) Kind() string {
	if strings.TrimSpace(r.WorkloadKind) != "" {
		return strings.TrimSpace(r.WorkloadKind)
	}
	return TypeGeneric
}

func (r *GenericRuntime) resolvedName() string {
	obj, err := r.load()
	if err != nil {
		return ""
	}
	return obj.GetName()
}

func (r *GenericRuntime) WorkloadName() string {
	name := r.resolvedName()
	if name == "" {
		slog.Warn("failed to resolve workload name from manifest", "path", r.ManifestPath)
	}
	return name
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
	name := obj.GetName()
	workloadLabels := LabelsWithWorkload(r.Labels, name, obj.GetKind())
	workloadAnnotations := AnnotationsWithWorkload(r.Annotations, name, obj.GetAPIVersion(), obj.GetKind())
	obj.SetLabels(mergeStringMaps(obj.GetLabels(), workloadLabels))
	obj.SetAnnotations(mergeStringMaps(obj.GetAnnotations(), workloadAnnotations))
	if r.LastAppliedSpecJSON != "" {
		annos := obj.GetAnnotations()
		annos[AnnotationLastAppliedSpec] = r.LastAppliedSpecJSON
		annos[AnnotationLastAppliedHash] = r.LastAppliedSpecHash
		obj.SetAnnotations(annos)
	}
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
			if injectAttachable(inject) {
				templateLabels["okdev.io/mesh-role"] = "hub"
			} else {
				templateLabels["okdev.io/mesh-role"] = "receiver"
			}
			template.Spec, err = kube.PreparePodSpecForTargetWithShellAndSyncRoots(template.Spec, r.Volumes, r.WorkspaceMountPath, r.SidecarImage, r.SidecarResources, r.Tmux, r.PreStop, r.interactiveContainer(), r.Shell, r.SyncRemoteRoots)
			if err != nil {
				return err
			}
		} else {
			kube.InjectPreStopForTarget(&template.Spec, r.PreStop, r.interactiveContainer())
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
	return k.DeleteByRef(ctx, namespace, obj.GetAPIVersion(), obj.GetKind(), r.resolvedName(), ignoreNotFound)
}

func (r *GenericRuntime) WaitReady(ctx context.Context, k WaitClient, namespace string, timeout time.Duration, onProgress func(kube.PodReadinessProgress)) error {
	return waitForCandidatePodReady(ctx, k, namespace, r.selectCandidate, timeout, onProgress,
		failFastOnPodFailureForWorkload(r.Kind()),
		fmt.Sprintf("wait for %s workload target pod readiness timed out", r.Kind()))
}

func failFastOnPodFailureForWorkload(kind string) bool {
	switch strings.ToLower(strings.TrimSpace(kind)) {
	case TypePod, TypeJob, TypePyTorchJob:
		return true
	default:
		return false
	}
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
		if podIsAttachable(pod) {
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

// podIsAttachable reports whether a pod may serve as the session target.
//
// A missing label means attachable, matching how `okdev status` has always read
// it. Only an explicit "false" excludes a pod, which is what multi-pod
// workloads stamp on their non-target replicas. The distinction matters on
// upgrade: pods created before okdev labelled attachability carry no label at
// all, and treating that as "not attachable" strands every existing pod session
// the moment its target has to be re-resolved.
func podIsAttachable(pod kube.PodSummary) bool {
	return !strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/attachable"]), "false")
}

// interactiveContainer is the container okdev attaches to.
//
// It resolves against the manifest with the same rule kube's injector uses:
// the configured name when a pod template has it, otherwise that template's
// first container. Injection already fell back that way; this did not, so a
// manifest whose only container is not named "dev" produced a correctly built
// pod that `okdev ssh` and `okdev exec` could not enter. One rule, both sides.
func (r *GenericRuntime) interactiveContainer() string {
	// A configured attach.container is honored as written. Overriding it would
	// silently ignore what the user asked for; if it names a container the
	// manifest lacks, that is a config error to surface, not to paper over.
	if configured := strings.TrimSpace(r.TargetContainer); configured != "" {
		return configured
	}
	// Nothing configured: fall back the same way the injector does, so exec
	// reaches the container that actually received the workspace mount.
	names := r.injectedContainerNames()
	for _, name := range names {
		if name == DefaultTargetContainer {
			return DefaultTargetContainer
		}
	}
	if len(names) > 0 {
		return names[0]
	}
	return DefaultTargetContainer
}

// injectedContainerNames lists the containers of the first injected pod
// template. An unreadable manifest yields nothing, leaving the caller with the
// configured name — the manifest's own errors are reported elsewhere.
func (r *GenericRuntime) injectedContainerNames() []string {
	obj, err := r.load()
	if err != nil {
		return nil
	}
	for _, inject := range r.Inject {
		templateMap, err := resolveMapPath(obj.Object, inject.Path)
		if err != nil {
			continue
		}
		template, err := decodePodTemplateSpec(templateMap)
		if err != nil {
			continue
		}
		if len(template.Spec.Containers) == 0 {
			continue
		}
		names := make([]string, 0, len(template.Spec.Containers))
		for _, c := range template.Spec.Containers {
			names = append(names, c.Name)
		}
		return names
	}
	return nil
}

func (r *GenericRuntime) load() (*unstructured.Unstructured, error) {
	r.loadMu.Lock()
	defer r.loadMu.Unlock()
	var raw []byte
	if len(r.ManifestBytes) > 0 {
		if r.loadedBase != nil && r.loadedFrom == (manifestCacheStamp{}) {
			return r.loadedBase.DeepCopy(), nil
		}
		raw = r.ManifestBytes
	} else {
		stamp, err := manifestStamp(r.ManifestPath)
		if err != nil {
			return nil, fmt.Errorf("stat generic manifest %q: %w", r.ManifestPath, err)
		}
		if r.loadedBase != nil && r.loadedFrom == stamp {
			return r.loadedBase.DeepCopy(), nil
		}
		fileRaw, err := os.ReadFile(r.ManifestPath)
		if err != nil {
			return nil, fmt.Errorf("read generic manifest %q: %w", r.ManifestPath, err)
		}
		rendered, err := r.renderManifestTemplate(fileRaw)
		if err != nil {
			return nil, err
		}
		raw = rendered
		defer func() { r.loadedFrom = stamp }()
	}

	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		return nil, fmt.Errorf("parse workload manifest %q: %w", r.manifestSource(), err)
	}
	u := &unstructured.Unstructured{Object: obj}
	if strings.TrimSpace(u.GetAPIVersion()) == "" || strings.TrimSpace(u.GetKind()) == "" {
		return nil, fmt.Errorf("workload manifest %q is missing apiVersion/kind", r.manifestSource())
	}
	if strings.TrimSpace(u.GetName()) == "" {
		return nil, fmt.Errorf("workload manifest %q is missing metadata.name", r.manifestSource())
	}
	r.loadedBase = u.DeepCopy()
	return u.DeepCopy(), nil
}

// manifestSource names the manifest in error messages: a path when one was
// given, otherwise the synthesized-manifest marker.
func (r *GenericRuntime) manifestSource() string {
	if len(r.ManifestBytes) > 0 {
		return "<synthesized " + r.Kind() + ">"
	}
	return r.ManifestPath
}

type WorkloadManifestTemplateVars struct {
	WorkloadName string
	SessionName  string
	RunID        string
	ConfigName   string
	WorkloadType string
}

func (r *GenericRuntime) renderManifestTemplate(raw []byte) ([]byte, error) {
	tmpl, err := template.New("workload-manifest").Option("missingkey=error").Parse(string(raw))
	if err != nil {
		return nil, fmt.Errorf("parse workload manifest template %q: %w", r.ManifestPath, err)
	}
	var out bytes.Buffer
	vars := WorkloadManifestTemplateVars{
		WorkloadName: strings.TrimSpace(r.WorkloadNameOverride),
		SessionName:  strings.TrimSpace(r.SessionName),
		RunID:        strings.TrimSpace(r.Labels["okdev.io/run-id"]),
		ConfigName:   strings.TrimSpace(r.Labels["okdev.io/name"]),
		WorkloadType: r.Kind(),
	}
	if err := tmpl.Execute(&out, vars); err != nil {
		return nil, fmt.Errorf("render workload manifest template %q: %w", r.ManifestPath, err)
	}
	return out.Bytes(), nil
}

func manifestStamp(path string) (manifestCacheStamp, error) {
	info, err := os.Stat(path)
	if err != nil {
		return manifestCacheStamp{}, err
	}
	return manifestCacheStamp{
		path:    path,
		modTime: info.ModTime(),
		size:    info.Size(),
	}, nil
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
// The empty path addresses the object root, where the pod template is the
// object itself (a bare Pod); there the members are merged in so apiVersion
// and kind — which are not part of a PodTemplateSpec — survive the round trip.
func writeMapPath(root map[string]any, path string, value map[string]any) error {
	if strings.TrimSpace(path) == "" {
		for k, v := range value {
			root[k] = v
		}
		return nil
	}
	parts := strings.Split(strings.TrimSpace(path), ".")
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
