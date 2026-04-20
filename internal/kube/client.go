package kube

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/shellutil"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apiequality "k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/discovery"
	"k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
	"sigs.k8s.io/yaml"
)

var chooseContainerErrPattern = regexp.MustCompile(`choose one of: \[([^\]]+)\]`)

var deletionWaitTimeout = 5 * time.Minute

type Client struct {
	Context string

	mu            sync.Mutex
	cachedSet     *kubernetes.Clientset
	cachedDynamic dynamic.Interface
	cachedMapper  meta.RESTMapper
	cachedConfig  *rest.Config
	cachedKey     string
}

type PodSummary struct {
	Namespace   string
	Name        string
	Phase       string
	Deleting    bool
	CreatedAt   time.Time
	Labels      map[string]string
	Annotations map[string]string
	Ready       string
	Restarts    int32
	Reason      string
	PodIP       string
}

type PodReadinessProgress struct {
	Phase           corev1.PodPhase
	ReadyContainers int
	TotalContainers int
	Reason          string
}

type LogStreamOptions struct {
	Container string
	Follow    bool
	Previous  bool
	TailLines *int64
	Since     time.Duration
}

func (c *Client) restConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	overrides := &clientcmd.ConfigOverrides{}
	if c.Context != "" {
		overrides.CurrentContext = c.Context
	}
	cfg := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, overrides)
	restCfg, err := cfg.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("load kube config: %w", err)
	}
	restCfg.Timeout = 0 // Disable default timeout; use context for all operations.
	restCfg.Dial = (&net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 10 * time.Second,
	}).DialContext
	return restCfg, nil
}

func (c *Client) clients() (*kubernetes.Clientset, dynamic.Interface, meta.RESTMapper, *rest.Config, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cacheKey, err := c.cacheKey()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	if c.cachedSet != nil && c.cachedConfig != nil && c.cachedDynamic != nil && c.cachedMapper != nil && c.cachedKey == cacheKey {
		return c.cachedSet, c.cachedDynamic, c.cachedMapper, c.cachedConfig, nil
	}
	restCfg, err := c.restConfig()
	if err != nil {
		return nil, nil, nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("create kubernetes client: %w", err)
	}
	dc, err := dynamic.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("create dynamic client: %w", err)
	}
	discoveryClient, err := discovery.NewDiscoveryClientForConfig(restCfg)
	if err != nil {
		return nil, nil, nil, nil, fmt.Errorf("create discovery client: %w", err)
	}
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(memory.NewMemCacheClient(discoveryClient))
	c.cachedSet = cs
	c.cachedDynamic = dc
	c.cachedMapper = mapper
	c.cachedConfig = restCfg
	c.cachedKey = cacheKey
	return cs, dc, mapper, restCfg, nil
}

func (c *Client) cacheKey() (string, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	parts := []string{strings.TrimSpace(c.Context)}
	for _, path := range loadingRules.Precedence {
		info, err := os.Stat(path)
		if err == nil {
			parts = append(parts, fmt.Sprintf("%s:%d:%d", path, info.ModTime().UnixNano(), info.Size()))
			continue
		}
		if errors.Is(err, os.ErrNotExist) {
			parts = append(parts, path+":missing")
			continue
		}
		return "", fmt.Errorf("stat kubeconfig %q: %w", path, err)
	}
	return strings.Join(parts, "|"), nil
}

func (c *Client) clientset() (*kubernetes.Clientset, *rest.Config, error) {
	cs, _, _, cfg, err := c.clients()
	if err != nil {
		return nil, nil, err
	}
	return cs, cfg, nil
}

func (c *Client) Apply(ctx context.Context, namespace string, manifest []byte) error {
	var tm metav1.TypeMeta
	if err := yaml.Unmarshal(manifest, &tm); err != nil {
		return fmt.Errorf("decode manifest type: %w", err)
	}
	cs, dc, mapper, _, err := c.clients()
	if err != nil {
		return err
	}

	switch tm.Kind {
	case "Pod":
		var pod corev1.Pod
		if err := yaml.Unmarshal(manifest, &pod); err != nil {
			return fmt.Errorf("decode pod manifest: %w", err)
		}
		pod.Namespace = namespace
		existing, err := cs.CoreV1().Pods(namespace).Get(ctx, pod.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cs.CoreV1().Pods(namespace).Create(ctx, &pod, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		if existing.DeletionTimestamp != nil {
			if err := waitForPodDeletion(ctx, cs, namespace, pod.Name); err != nil {
				return err
			}
			_, err = cs.CoreV1().Pods(namespace).Create(ctx, &pod, metav1.CreateOptions{})
			return err
		}
		if apiequality.Semantic.DeepEqual(existing.Spec, pod.Spec) &&
			maps.Equal(existing.Labels, pod.Labels) &&
			maps.Equal(existing.Annotations, pod.Annotations) {
			return nil
		}
		if err := cs.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
			return err
		}
		if err := waitForPodDeletion(ctx, cs, namespace, pod.Name); err != nil {
			return err
		}
		_, err = cs.CoreV1().Pods(namespace).Create(ctx, &pod, metav1.CreateOptions{})
		return err
	case "PersistentVolumeClaim":
		var pvc corev1.PersistentVolumeClaim
		if err := yaml.Unmarshal(manifest, &pvc); err != nil {
			return fmt.Errorf("decode pvc manifest: %w", err)
		}
		pvc.Namespace = namespace
		existing, err := cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, pvc.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cs.CoreV1().PersistentVolumeClaims(namespace).Create(ctx, &pvc, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}

		mutableSpecChanged, immutableSpecChanged := pvcSpecDiff(existing.Spec, pvc.Spec)
		if immutableSpecChanged {
			if mutableSpecChanged {
				return fmt.Errorf("pvc/%s has immutable spec changes (and mutable resource updates); delete and recreate the PVC (or use --delete-pvc with `okdev down`) to apply", pvc.Name)
			}
			return fmt.Errorf("pvc/%s has immutable spec changes; delete and recreate the PVC (or use --delete-pvc with `okdev down`) to apply", pvc.Name)
		}

		needsMetaUpdate := !maps.Equal(existing.Labels, pvc.Labels) || !maps.Equal(existing.Annotations, pvc.Annotations)
		if !needsMetaUpdate && !mutableSpecChanged {
			return nil
		}

		updated := existing.DeepCopy()
		updated.Labels = cloneStringMap(pvc.Labels)
		updated.Annotations = cloneStringMap(pvc.Annotations)
		if mutableSpecChanged {
			updated.Spec.Resources = pvc.Spec.Resources
			updated.Spec.VolumeAttributesClassName = pvc.Spec.VolumeAttributesClassName
		}
		_, err = cs.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, updated, metav1.UpdateOptions{})
		return err
	case "Job":
		var job batchv1.Job
		if err := yaml.Unmarshal(manifest, &job); err != nil {
			return fmt.Errorf("decode job manifest: %w", err)
		}
		job.Namespace = namespace
		normalizeJobForApplyComparison(&job)
		existing, err := cs.BatchV1().Jobs(namespace).Get(ctx, job.Name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			_, err = cs.BatchV1().Jobs(namespace).Create(ctx, &job, metav1.CreateOptions{})
			return err
		}
		if err != nil {
			return err
		}
		normalizedExisting := existing.DeepCopy()
		normalizeJobForApplyComparison(normalizedExisting)
		if apiequality.Semantic.DeepEqual(normalizedExisting.Spec, job.Spec) &&
			maps.Equal(normalizedExisting.Labels, job.Labels) &&
			maps.Equal(normalizedExisting.Annotations, job.Annotations) {
			return nil
		}
		propagation := metav1.DeletePropagationBackground
		if err := cs.BatchV1().Jobs(namespace).Delete(ctx, job.Name, metav1.DeleteOptions{PropagationPolicy: &propagation}); err != nil {
			return err
		}
		if err := waitForJobDeletion(ctx, cs, namespace, job.Name); err != nil {
			return err
		}
		_, err = cs.BatchV1().Jobs(namespace).Create(ctx, &job, metav1.CreateOptions{})
		return err
	default:
		return c.applyUnstructured(ctx, dc, mapper, namespace, manifest)
	}
}

func (c *Client) applyUnstructured(ctx context.Context, dc dynamic.Interface, mapper meta.RESTMapper, namespace string, manifest []byte) error {
	var obj unstructured.Unstructured
	if err := yaml.Unmarshal(manifest, &obj.Object); err != nil {
		return fmt.Errorf("decode unstructured manifest: %w", err)
	}
	gvk := obj.GroupVersionKind()
	if gvk.Empty() {
		return fmt.Errorf("manifest missing apiVersion/kind")
	}
	if strings.TrimSpace(obj.GetName()) == "" {
		return fmt.Errorf("manifest missing metadata.name")
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gvk.Group, Kind: gvk.Kind}, gvk.Version)
	if err != nil {
		return fmt.Errorf("resolve rest mapping for %s: %w", gvk.String(), err)
	}
	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		obj.SetNamespace(namespace)
		resource = dc.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = dc.Resource(mapping.Resource)
	}
	existing, err := resource.Get(ctx, obj.GetName(), metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		_, err = resource.Create(ctx, &obj, metav1.CreateOptions{})
		return err
	}
	if err != nil {
		return err
	}
	obj.SetResourceVersion(existing.GetResourceVersion())
	_, err = resource.Update(ctx, &obj, metav1.UpdateOptions{})
	return err
}

func waitForPodDeletion(ctx context.Context, cs *kubernetes.Clientset, namespace, name string) error {
	ctx, cancel := deletionWaitContext(ctx)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func waitForJobDeletion(ctx context.Context, cs *kubernetes.Clientset, namespace, name string) error {
	ctx, cancel := deletionWaitContext(ctx)
	defer cancel()

	ticker := time.NewTicker(300 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func deletionWaitContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if _, ok := ctx.Deadline(); ok {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, deletionWaitTimeout)
}

func normalizeJobForApplyComparison(job *batchv1.Job) {
	if job == nil {
		return
	}
	scheme.Scheme.Default(job)

	job.ResourceVersion = ""
	job.UID = ""
	job.CreationTimestamp = metav1.Time{}
	job.Generation = 0
	job.ManagedFields = nil
	job.SelfLink = ""

	if !manualSelectorEnabled(job.Spec.ManualSelector) {
		job.Spec.Selector = nil
		if job.Spec.Template.Labels != nil {
			for _, key := range []string{
				"batch.kubernetes.io/controller-uid",
				"batch.kubernetes.io/job-name",
				"controller-uid",
				"job-name",
			} {
				delete(job.Spec.Template.Labels, key)
			}
			if len(job.Spec.Template.Labels) == 0 {
				job.Spec.Template.Labels = nil
			}
		}
	}
	if job.Spec.Parallelism != nil && *job.Spec.Parallelism == 1 {
		job.Spec.Parallelism = nil
	}
	if job.Spec.Completions != nil && *job.Spec.Completions == 1 {
		job.Spec.Completions = nil
	}
	if job.Spec.ManualSelector != nil && !*job.Spec.ManualSelector {
		job.Spec.ManualSelector = nil
	}
	if job.Spec.CompletionMode != nil && *job.Spec.CompletionMode == batchv1.NonIndexedCompletion {
		job.Spec.CompletionMode = nil
	}
	if job.Spec.Suspend != nil && !*job.Spec.Suspend {
		job.Spec.Suspend = nil
	}
	if job.Spec.PodReplacementPolicy != nil && *job.Spec.PodReplacementPolicy == batchv1.TerminatingOrFailed {
		job.Spec.PodReplacementPolicy = nil
	}
	normalizePodSpecDefaults(&job.Spec.Template.Spec)
}

func manualSelectorEnabled(v *bool) bool {
	return v != nil && *v
}

func normalizePodSpecDefaults(spec *corev1.PodSpec) {
	if spec == nil {
		return
	}
	if spec.TerminationGracePeriodSeconds != nil && *spec.TerminationGracePeriodSeconds == 30 {
		spec.TerminationGracePeriodSeconds = nil
	}
	if spec.DNSPolicy == corev1.DNSClusterFirst {
		spec.DNSPolicy = ""
	}
	if spec.SchedulerName == "default-scheduler" {
		spec.SchedulerName = ""
	}
	if spec.SecurityContext != nil && apiequality.Semantic.DeepEqual(*spec.SecurityContext, corev1.PodSecurityContext{}) {
		spec.SecurityContext = nil
	}
	for i := range spec.Containers {
		normalizeContainerDefaults(&spec.Containers[i])
	}
}

func normalizeContainerDefaults(c *corev1.Container) {
	if c == nil {
		return
	}
	if c.ImagePullPolicy == corev1.PullIfNotPresent {
		c.ImagePullPolicy = ""
	}
	if c.TerminationMessagePath == "/dev/termination-log" {
		c.TerminationMessagePath = ""
	}
	if c.TerminationMessagePolicy == corev1.TerminationMessageReadFile {
		c.TerminationMessagePolicy = ""
	}
	for i := range c.Ports {
		if c.Ports[i].Protocol == corev1.ProtocolTCP {
			c.Ports[i].Protocol = ""
		}
	}
}

func pvcSpecDiff(existing, desired corev1.PersistentVolumeClaimSpec) (mutableChanged bool, immutableChanged bool) {
	if apiequality.Semantic.DeepEqual(existing, desired) {
		return false, false
	}
	mutableChanged = !apiequality.Semantic.DeepEqual(existing.Resources, desired.Resources) ||
		!apiequality.Semantic.DeepEqual(existing.VolumeAttributesClassName, desired.VolumeAttributesClassName)

	existingComparable := existing.DeepCopy()
	desiredComparable := desired.DeepCopy()
	existingComparable.Resources = desiredComparable.Resources
	existingComparable.VolumeAttributesClassName = desiredComparable.VolumeAttributesClassName
	normalizeComparablePVCSpec(existingComparable, desiredComparable)
	if apiequality.Semantic.DeepEqual(existingComparable, desiredComparable) {
		return mutableChanged, false
	}
	return mutableChanged, true
}

func normalizeComparablePVCSpec(existing, desired *corev1.PersistentVolumeClaimSpec) {
	// Kubernetes binds and defaults these fields server-side; if user didn't
	// specify them in desired spec, don't treat this as immutable drift.
	if desired.StorageClassName == nil {
		desired.StorageClassName = existing.StorageClassName
	}
	if desired.VolumeMode == nil {
		desired.VolumeMode = existing.VolumeMode
	}
	desired.VolumeName = existing.VolumeName
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return map[string]string{}
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (c *Client) Delete(ctx context.Context, namespace string, kind string, name string, ignoreNotFound bool) error {
	return c.DeleteByRef(ctx, namespace, "", kind, name, ignoreNotFound)
}

func (c *Client) ResourceExists(ctx context.Context, namespace string, apiVersion string, kind string, name string) (bool, error) {
	if strings.TrimSpace(name) == "" {
		return false, fmt.Errorf("resource name is required")
	}

	cs, dc, mapper, _, err := c.clients()
	if err != nil {
		return false, err
	}

	switch {
	case strings.EqualFold(strings.TrimSpace(apiVersion), "v1") && strings.EqualFold(strings.TrimSpace(kind), "pod"):
		_, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	case strings.EqualFold(strings.TrimSpace(apiVersion), "batch/v1") && strings.EqualFold(strings.TrimSpace(kind), "job"):
		_, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}

	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return false, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
	}
	mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
	if err != nil {
		return false, fmt.Errorf("resolve rest mapping for %s/%s: %w", apiVersion, kind, err)
	}
	var resource dynamic.ResourceInterface
	if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
		resource = dc.Resource(mapping.Resource).Namespace(namespace)
	} else {
		resource = dc.Resource(mapping.Resource)
	}
	_, err = resource.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	return err == nil, err
}

func (c *Client) GetResourceAnnotation(ctx context.Context, namespace, apiVersion, kind, name, key string) (string, bool, error) {
	if strings.TrimSpace(name) == "" {
		return "", false, fmt.Errorf("resource name is required")
	}

	cs, dc, mapper, _, err := c.clients()
	if err != nil {
		return "", false, err
	}

	var annotations map[string]string

	switch {
	case strings.EqualFold(strings.TrimSpace(apiVersion), "v1") && strings.EqualFold(strings.TrimSpace(kind), "pod"):
		obj, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", false, err
		}
		annotations = obj.Annotations
	case strings.EqualFold(strings.TrimSpace(apiVersion), "batch/v1") && strings.EqualFold(strings.TrimSpace(kind), "job"):
		obj, err := cs.BatchV1().Jobs(namespace).Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", false, err
		}
		annotations = obj.Annotations
	default:
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return "", false, fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
		}
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
		if err != nil {
			return "", false, fmt.Errorf("resolve rest mapping for %s/%s: %w", apiVersion, kind, err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = dc.Resource(mapping.Resource).Namespace(namespace)
		} else {
			resource = dc.Resource(mapping.Resource)
		}
		obj, err := resource.Get(ctx, name, metav1.GetOptions{})
		if err != nil {
			return "", false, err
		}
		annotations = obj.GetAnnotations()
	}

	v, ok := annotations[key]
	return v, ok, nil
}

func (c *Client) DeleteByRef(ctx context.Context, namespace string, apiVersion string, kind string, name string, ignoreNotFound bool) error {
	cs, dc, mapper, _, err := c.clients()
	if err != nil {
		return err
	}

	var delErr error
	switch strings.ToLower(kind) {
	case "pod", "pods":
		delErr = cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "job", "jobs":
		propagation := metav1.DeletePropagationBackground
		delErr = cs.BatchV1().Jobs(namespace).Delete(ctx, name, metav1.DeleteOptions{PropagationPolicy: &propagation})
	case "pvc", "persistentvolumeclaim", "persistentvolumeclaims":
		delErr = cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	default:
		if strings.TrimSpace(apiVersion) == "" {
			return fmt.Errorf("unsupported delete kind %q", kind)
		}
		gv, err := schema.ParseGroupVersion(apiVersion)
		if err != nil {
			return fmt.Errorf("parse apiVersion %q: %w", apiVersion, err)
		}
		mapping, err := mapper.RESTMapping(schema.GroupKind{Group: gv.Group, Kind: kind}, gv.Version)
		if err != nil {
			return fmt.Errorf("resolve rest mapping for %s/%s: %w", apiVersion, kind, err)
		}
		var resource dynamic.ResourceInterface
		if mapping.Scope.Name() == meta.RESTScopeNameNamespace {
			resource = dc.Resource(mapping.Resource).Namespace(namespace)
		} else {
			resource = dc.Resource(mapping.Resource)
		}
		delErr = resource.Delete(ctx, name, metav1.DeleteOptions{})
	}
	if ignoreNotFound && apierrors.IsNotFound(delErr) {
		return nil
	}
	return delErr
}

func (c *Client) WaitReady(ctx context.Context, namespace, pod string, timeout time.Duration) error {
	return c.WaitReadyWithProgress(ctx, namespace, pod, timeout, nil)
}

func (c *Client) WaitReadyWithProgress(ctx context.Context, namespace, pod string, timeout time.Duration, onProgress func(PodReadinessProgress)) error {
	ctxWait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cs, _, _, _, err := c.clients()
	if err != nil {
		return err
	}

	current, err := cs.CoreV1().Pods(namespace).Get(ctxWait, pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	var lastProgress *PodReadinessProgress
	emitProgress := func(p *corev1.Pod) {
		progress := podProgress(p)
		if onProgress != nil && (lastProgress == nil || *lastProgress != progress) {
			onProgress(progress)
			copyProgress := progress
			lastProgress = &copyProgress
		}
	}
	emitProgress(current)
	if isPodReady(current) {
		return nil
	}
	resourceVersion := current.ResourceVersion

	for {
		watcher, err := cs.CoreV1().Pods(namespace).Watch(ctxWait, metav1.ListOptions{
			FieldSelector:   fields.OneTermEqualSelector("metadata.name", pod).String(),
			ResourceVersion: resourceVersion,
		})
		if err != nil {
			return err
		}
		restartWatch := false
		for !restartWatch {
			select {
			case <-ctxWait.Done():
				watcher.Stop()
				return fmt.Errorf("wait for pod/%s ready: %w", pod, ctxWait.Err())
			case evt, ok := <-watcher.ResultChan():
				if !ok {
					restartWatch = true
					continue
				}
				switch evt.Type {
				case watch.Added, watch.Modified:
					p, ok := evt.Object.(*corev1.Pod)
					if !ok {
						continue
					}
					resourceVersion = p.ResourceVersion
					emitProgress(p)
					if p.DeletionTimestamp != nil {
						watcher.Stop()
						return fmt.Errorf("pod/%s is terminating", pod)
					}
					if isPodReady(p) {
						watcher.Stop()
						return nil
					}
				case watch.Deleted:
					watcher.Stop()
					return fmt.Errorf("pod/%s was deleted while waiting for readiness", pod)
				case watch.Error:
					restartWatch = true
				}
			}
		}
		watcher.Stop()
	}
}

func isPodReady(p *corev1.Pod) bool {
	for _, cond := range p.Status.Conditions {
		if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func podProgress(p *corev1.Pod) PodReadinessProgress {
	progress := PodReadinessProgress{
		Phase:           p.Status.Phase,
		ReadyContainers: 0,
		TotalContainers: len(p.Status.ContainerStatuses),
		Reason:          p.Status.Reason,
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Ready {
			progress.ReadyContainers++
			continue
		}
		if c.State.Waiting != nil && c.State.Waiting.Reason != "" {
			progress.Reason = c.State.Waiting.Reason
		}
	}
	if progress.Reason == "" {
		progress.Reason = "-"
	}
	return progress
}

func (c *Client) ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]PodSummary, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return nil, err
	}

	ns := namespace
	if allNamespaces {
		ns = ""
	}
	pods, err := cs.CoreV1().Pods(ns).List(ctx, metav1.ListOptions{LabelSelector: labelSelector})
	if err != nil {
		return nil, err
	}
	out := make([]PodSummary, 0, len(pods.Items))
	for _, p := range pods.Items {
		out = append(out, podSummaryFromPod(&p))
	}
	return out, nil
}

func (c *Client) GetPodSummary(ctx context.Context, namespace, name string) (*PodSummary, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return nil, err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	s := podSummaryFromPod(p)
	return &s, nil
}

// ContainerRestartInfo holds restart details for a single container.
type ContainerRestartInfo struct {
	RestartCount   int32
	LastExitCode   int32
	LastReason     string // e.g. "OOMKilled", "Error"
	LastMessage    string
	CurrentWaiting string // e.g. "CrashLoopBackOff"
}

// GetContainerRestartInfo returns restart information for a specific container
// in a pod. Returns nil if the container is not found.
func (c *Client) GetContainerRestartInfo(ctx context.Context, namespace, pod, container string) (*ContainerRestartInfo, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return nil, err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	for _, st := range p.Status.ContainerStatuses {
		if st.Name != container {
			continue
		}
		info := &ContainerRestartInfo{
			RestartCount: st.RestartCount,
		}
		if t := st.LastTerminationState.Terminated; t != nil {
			info.LastExitCode = t.ExitCode
			info.LastReason = t.Reason
			info.LastMessage = t.Message
		}
		if w := st.State.Waiting; w != nil {
			info.CurrentWaiting = w.Reason
		}
		return info, nil
	}
	return nil, nil
}

func (c *Client) PersistentVolumeClaimExists(ctx context.Context, namespace, name string) (bool, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return false, err
	}
	_, err = cs.CoreV1().PersistentVolumeClaims(namespace).Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (c *Client) TouchPodActivity(ctx context.Context, namespace, pod string) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{"okdev.io/last-attach":"%s"}}}`, time.Now().UTC().Format(time.RFC3339)))
	_, err = cs.CoreV1().Pods(namespace).Patch(ctx, pod, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) AnnotatePod(ctx context.Context, namespace, pod, key, value string) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
	patch := []byte(fmt.Sprintf(`{"metadata":{"annotations":{%q:%q}}}`, key, value))
	_, err = cs.CoreV1().Pods(namespace).Patch(ctx, pod, types.MergePatchType, patch, metav1.PatchOptions{})
	return err
}

func (c *Client) GetPodAnnotation(ctx context.Context, namespace, pod, key string) (string, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return "", err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	return p.Annotations[key], nil
}

func (c *Client) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(command) == 0 {
		return errors.New("exec command cannot be empty")
	}
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	err = c.execStream(ctx, cs, cfg, namespace, pod, "", command, stdin, stdout, stderr, tty)
	if err == nil {
		return nil
	}
	container := preferredContainerFromExecErr(err)
	if container == "" {
		return err
	}
	return c.execStream(ctx, cs, cfg, namespace, pod, container, command, stdin, stdout, stderr, tty)
}

func (c *Client) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(command) == 0 {
		return errors.New("exec command cannot be empty")
	}
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	return c.execStream(ctx, cs, cfg, namespace, pod, container, command, stdin, stdout, stderr, tty)
}

func (c *Client) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	for {
		start := time.Now()
		err := c.portForwardOnce(ctx, namespace, pod, forwards, stdout, stderr)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil && !isRetryablePortForwardError(err) {
			return err
		}
		// Reset backoff if the connection was healthy for a while.
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		if err != nil {
			slog.Debug("port-forward connection lost, reconnecting", "error", err, "backoff", backoff)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func isRetryablePortForwardError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	nonRetryable := []string{
		"unable to listen on any of the requested ports",
		"address already in use",
		"forbidden",
		"unauthorized",
		"not found",
		"bad request",
		"invalid",
	}
	for _, s := range nonRetryable {
		if strings.Contains(msg, s) {
			return false
		}
	}
	// Default to retryable for unknown errors; the non-retryable list
	// above already guards against permanent failures.
	return true
}

func (c *Client) portForwardOnce(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	reqURL, err := c.portForwardURL(cs, namespace, pod)
	if err != nil {
		return err
	}
	transport, upgrader, err := spdy.RoundTripperFor(cfg)
	if err != nil {
		return err
	}
	var dialer httpstream.Dialer = spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)
	tunnelingDialer, tErr := portforward.NewSPDYOverWebsocketDialer(reqURL, cfg)
	if tErr == nil {
		dialer = portforward.NewFallbackDialer(tunnelingDialer, dialer, httpstream.IsUpgradeFailure)
	}

	stopCh := make(chan struct{})
	readyCh := make(chan struct{})
	go func() {
		<-ctx.Done()
		close(stopCh)
	}()
	pf, err := portforward.New(dialer, forwards, stopCh, readyCh, stdout, stderr)
	if err != nil {
		return err
	}
	return pf.ForwardPorts()
}

// CopyProgress provides optional per-file and byte-level callbacks so callers
// can render transfer progress without the kube package knowing about UI.
// All fields are optional. Implementations are invoked from the goroutine
// doing the transfer and should be non-blocking.
type CopyProgress struct {
	// OnFileStart is called when a new file (or directory entry) begins
	// transfer. size is -1 if unknown.
	OnFileStart func(name string, size int64)
	// OnBytes is called with the number of bytes that just moved. It may be
	// called many times per file.
	OnBytes func(n int)
	// OnFileEnd is called when the current file finishes.
	OnFileEnd func(name string)
	// OnPhase is called when the copy moves between long-running phases (e.g.
	// enumerating remote files before the first byte lands). An empty phase
	// signals the renderer to clear any previous phase message. OnPhase may be
	// nil and callers should use (*CopyProgress).phase to invoke it safely.
	OnPhase func(phase string)
}

// CopyOptions holds optional behaviour flags for copy operations.
type CopyOptions struct {
	Progress *CopyProgress
	// Compress gzip-compresses the tar stream on the wire. For directory
	// copies this wires `tar czf -`/`tar xzf -` on the pod side and a
	// gzip.Reader/gzip.Writer on the local side. It assumes the remote image
	// ships `gzip` (ubiquitous in both GNU and busybox tar builds). Useful on
	// high-latency / low-bandwidth links where the exec relay is the
	// bottleneck; redundant (and a mild CPU cost) on fast links or on
	// pre-compressed payloads.
	Compress bool
}

func (p *CopyProgress) fileStart(name string, size int64) {
	if p == nil || p.OnFileStart == nil {
		return
	}
	p.OnFileStart(name, size)
}

func (p *CopyProgress) bytes(n int) {
	if p == nil || p.OnBytes == nil || n <= 0 {
		return
	}
	p.OnBytes(n)
}

func (p *CopyProgress) fileEnd(name string) {
	if p == nil || p.OnFileEnd == nil {
		return
	}
	p.OnFileEnd(name)
}

func (p *CopyProgress) phase(s string) {
	if p == nil || p.OnPhase == nil {
		return
	}
	p.OnPhase(s)
}

// countingReader wraps an io.Reader and reports byte counts to a CopyProgress.
type countingReader struct {
	r        io.Reader
	progress *CopyProgress
}

func (cr *countingReader) Read(p []byte) (int, error) {
	n, err := cr.r.Read(p)
	if n > 0 {
		cr.progress.bytes(n)
	}
	return n, err
}

// countingWriter wraps an io.Writer and reports byte counts to a CopyProgress.
type countingWriter struct {
	w        io.Writer
	progress *CopyProgress
}

func (cw *countingWriter) Write(p []byte) (int, error) {
	n, err := cw.w.Write(p)
	if n > 0 {
		cw.progress.bytes(n)
	}
	return n, err
}

func (c *Client) CopyToPod(ctx context.Context, namespace, localPath, podName, remotePath string) error {
	return c.CopyToPodInContainer(ctx, namespace, localPath, podName, "", remotePath)
}

func (c *Client) CopyToPodInContainer(ctx context.Context, namespace, localPath, podName, container, remotePath string) error {
	return c.CopyToPodInContainerWithOptions(ctx, namespace, localPath, podName, container, remotePath, CopyOptions{})
}

func (c *Client) CopyToPodInContainerWithOptions(ctx context.Context, namespace, localPath, podName, container, remotePath string, opts CopyOptions) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	var size int64 = -1
	if info, err := f.Stat(); err == nil {
		size = info.Size()
	}
	opts.Progress.fileStart(filepath.Base(localPath), size)
	var reader io.Reader = f
	if opts.Progress != nil {
		reader = &countingReader{r: f, progress: opts.Progress}
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("cat > %s", shellQuote(remotePath))}
	if err := c.execStream(ctx, cs, cfg, namespace, podName, container, cmd, reader, io.Discard, io.Discard, false); err != nil {
		return err
	}
	opts.Progress.fileEnd(filepath.Base(localPath))
	return nil
}

func (c *Client) ExtractTarToPod(ctx context.Context, namespace, podName, remoteDir string, tarStream io.Reader) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("mkdir -p %s && tar -xf - -C %s", shellQuote(remoteDir), shellQuote(remoteDir))}
	return c.execStream(ctx, cs, cfg, namespace, podName, "", cmd, tarStream, io.Discard, io.Discard, false)
}

func (c *Client) CopyFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	return c.CopyFromPodInContainer(ctx, namespace, podName, "", remotePath, localPath)
}

func (c *Client) CopyFromPodInContainer(ctx context.Context, namespace, podName, container, remotePath, localPath string) error {
	return c.CopyFromPodInContainerWithOptions(ctx, namespace, podName, container, remotePath, localPath, CopyOptions{})
}

func (c *Client) CopyFromPodInContainerWithOptions(ctx context.Context, namespace, podName, container, remotePath, localPath string, opts CopyOptions) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		produced, err := c.copyFromPodInContainerOnce(ctx, namespace, podName, container, remotePath, localPath, opts.Progress)
		if err == nil {
			return nil
		}
		lastErr = err
		// Don't retry once bytes have been written locally: restarting would
		// silently redo a partially-transferred large file.
		if produced || !isRetryableCopyStreamError(err) || attempt == 2 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return lastErr
}

// copyFromPodInContainerOnce streams a single remote file as a one-entry tar
// archive, extracts it to localPath, and reports whether any bytes were
// written locally (so the caller can skip retries after partial progress).
func (c *Client) copyFromPodInContainerOnce(ctx context.Context, namespace, podName, container, remotePath, localPath string, progress *CopyProgress) (produced bool, _ error) {
	cs, cfg, err := c.clientset()
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(remotePath)
	base := filepath.Base(remotePath)
	cmd := []string{"sh", "-lc", fmt.Sprintf("tar cf - -C %s %s", shellQuote(parent), shellQuote(base))}

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	execErrCh := make(chan error, 1)
	go func() {
		err := c.execStream(ctx, cs, cfg, namespace, podName, container, cmd, nil, pw, &errBuf, false)
		_ = pw.CloseWithError(err)
		execErrCh <- err
	}()

	extractProduced, extractErr := extractSingleFileFromTar(pr, localPath, progress)
	_ = pr.Close()
	execErr := <-execErrCh

	if extractErr != nil {
		return extractProduced, extractErr
	}
	if execErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return extractProduced, fmt.Errorf("%w: %s", execErr, msg)
		}
		return extractProduced, execErr
	}
	return extractProduced, nil
}

func createTempDownloadFile(localPath string) (*os.File, error) {
	dir := filepath.Dir(localPath)
	base := filepath.Base(localPath)
	return os.CreateTemp(dir, base+".tmp-*")
}

// extractSingleFileFromTar reads a one-entry tar stream and writes the file
// atomically to localPath. It reports whether any bytes were written so the
// caller can avoid retrying after partial progress.
func extractSingleFileFromTar(r io.Reader, localPath string, progress *CopyProgress) (produced bool, _ error) {
	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return false, err
	}
	tempFile, err := createTempDownloadFile(localPath)
	if err != nil {
		return false, err
	}
	tempPath := tempFile.Name()
	closed := false
	defer func() {
		if !closed {
			_ = tempFile.Close()
		}
		_ = os.Remove(tempPath)
	}()

	tr := tar.NewReader(r)
	foundFile := false
	var fileMode os.FileMode = 0o644
	var entryName string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return produced, err
		}
		switch hdr.Typeflag {
		case tar.TypeDir, tar.TypeXHeader, tar.TypeXGlobalHeader, tar.TypeGNULongName, tar.TypeGNULongLink:
			continue
		case tar.TypeReg, tar.TypeRegA:
			if foundFile {
				return produced, fmt.Errorf("tar archive contains multiple files")
			}
			entryName = filepath.Base(localPath)
			progress.fileStart(entryName, hdr.Size)
			dst := io.Writer(tempFile)
			if progress != nil {
				dst = &countingWriter{w: tempFile, progress: progress}
			}
			if _, err := io.Copy(dst, tr); err != nil {
				return produced, err
			}
			produced = true
			fileMode = os.FileMode(hdr.Mode)
			foundFile = true
			progress.fileEnd(entryName)
		default:
			return produced, fmt.Errorf("tar archive entry %q has unsupported type", hdr.Name)
		}
	}
	if !foundFile {
		return produced, fmt.Errorf("tar archive contained no file")
	}
	if err := tempFile.Chmod(fileMode); err != nil {
		return produced, err
	}
	if err := tempFile.Close(); err != nil {
		return produced, err
	}
	closed = true
	if err := os.Rename(tempPath, localPath); err != nil {
		return produced, err
	}
	return produced, nil
}

func (c *Client) StreamFromPod(ctx context.Context, namespace, podName, script string, stdout io.Writer) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	cmd := []string{"sh", "-lc", script}
	return c.execStream(ctx, cs, cfg, namespace, podName, "", cmd, nil, stdout, io.Discard, false)
}

// createTarFromDir writes a tar archive of dir's contents to w.
// Paths inside the archive are relative to dir. If progress is non-nil the
// callbacks are invoked as each regular file is archived.
func createTarFromDir(dir string, w io.Writer, progress *CopyProgress) error {
	tw := tar.NewWriter(w)
	defer tw.Close()
	return filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = rel
		if info.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		progress.fileStart(rel, info.Size())
		var src io.Reader = f
		if progress != nil {
			src = &countingReader{r: f, progress: progress}
		}
		if _, err := io.Copy(tw, src); err != nil {
			_ = f.Close()
			return err
		}
		progress.fileEnd(rel)
		return f.Close()
	})
}

// extractTarToDir extracts a tar archive from r into dir, reporting per-file
// progress via the optional callback. It returns whether any bytes were
// written locally so callers can avoid retrying after partial extraction.
func extractTarToDir(r io.Reader, dir string, progress *CopyProgress) (produced bool, _ error) {
	tr := tar.NewReader(r)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return produced, nil
		}
		if err != nil {
			return produced, err
		}
		target := filepath.Join(dir, hdr.Name)
		if !strings.HasPrefix(filepath.Clean(target), filepath.Clean(dir)+string(os.PathSeparator)) && filepath.Clean(target) != filepath.Clean(dir) {
			return produced, fmt.Errorf("tar entry %q escapes destination", hdr.Name)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)|0o755); err != nil {
				return produced, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return produced, err
			}
			f, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return produced, err
			}
			progress.fileStart(hdr.Name, hdr.Size)
			dst := io.Writer(f)
			if progress != nil {
				dst = &countingWriter{w: f, progress: progress}
			}
			if _, err := io.Copy(dst, tr); err != nil {
				_ = f.Close()
				return produced, err
			}
			produced = true
			progress.fileEnd(hdr.Name)
			if err := f.Close(); err != nil {
				return produced, err
			}
		}
	}
}

// CopyDirToPod archives a local directory and extracts it into remoteDir on the pod.
func (c *Client) CopyDirToPod(ctx context.Context, namespace, pod, container, localDir, remoteDir string) error {
	return c.CopyDirToPodWithOptions(ctx, namespace, pod, container, localDir, remoteDir, CopyOptions{})
}

func (c *Client) CopyDirToPodWithOptions(ctx context.Context, namespace, pod, container, localDir, remoteDir string, opts CopyOptions) error {
	pr, pw := io.Pipe()
	errCh := make(chan error, 1)
	// When compression is requested, gzip the tar on the local side (where CPU
	// is cheap and plentiful) before it reaches the SPDY wire. The pod-side
	// tar becomes `tar xzf -` to decompress on the fly.
	var tarSink io.WriteCloser = pw
	if opts.Compress {
		gz := gzip.NewWriter(pw)
		tarSink = &gzipPipeWriter{gz: gz, pw: pw}
	}
	go func() {
		err := createTarFromDir(localDir, tarSink, opts.Progress)
		if opts.Compress {
			if closeErr := tarSink.Close(); err == nil {
				err = closeErr
			}
		}
		_ = pw.Close()
		errCh <- err
	}()
	cs, cfg, err := c.clientset()
	if err != nil {
		pr.Close()
		return err
	}
	tarCmd := "tar xf -"
	if opts.Compress {
		tarCmd = "tar xzf -"
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("mkdir -p %s && %s -C %s", shellQuote(remoteDir), tarCmd, shellQuote(remoteDir))}
	if execErr := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, pr, io.Discard, io.Discard, false); execErr != nil {
		return execErr
	}
	return <-errCh
}

// gzipPipeWriter is a small adapter so the upload goroutine can Close() the
// gzip layer (flushing the trailing footer) independently of the underlying
// pipe writer; the pipe itself is closed in the goroutine epilogue so the
// exec stream sees EOF only after the gzip trailer has flushed.
type gzipPipeWriter struct {
	gz *gzip.Writer
	pw *io.PipeWriter
}

func (w *gzipPipeWriter) Write(p []byte) (int, error) { return w.gz.Write(p) }
func (w *gzipPipeWriter) Close() error                { return w.gz.Close() }

// CopyDirFromPod streams a remote directory as a tar archive and extracts it locally.
func (c *Client) CopyDirFromPod(ctx context.Context, namespace, pod, container, remoteDir, localDir string) error {
	return c.CopyDirFromPodWithOptions(ctx, namespace, pod, container, remoteDir, localDir, CopyOptions{})
}

func (c *Client) CopyDirFromPodWithOptions(ctx context.Context, namespace, pod, container, remoteDir, localDir string, opts CopyOptions) error {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		produced, err := c.copyDirFromPodOnce(ctx, namespace, pod, container, remoteDir, localDir, opts.Progress, opts.Compress)
		if err == nil {
			return nil
		}
		lastErr = err
		// Once files have landed on disk, retrying would restart a partially
		// completed transfer. Surface the error instead.
		if produced || !isRetryableCopyStreamError(err) || attempt == 2 {
			return err
		}
		time.Sleep(time.Duration(attempt+1) * 500 * time.Millisecond)
	}
	return lastErr
}

// copyDirFromPodOnce streams a remote tar archive directly into localDir,
// extracting files as they arrive so intermediate files become visible to the
// caller during the transfer. When compress is true the remote tar emits a
// gzipped archive and the local side decompresses on the fly.
func (c *Client) copyDirFromPodOnce(ctx context.Context, namespace, pod, container, remoteDir, localDir string, progress *CopyProgress, compress bool) (produced bool, _ error) {
	cs, cfg, err := c.clientset()
	if err != nil {
		return false, err
	}
	parent := filepath.Dir(remoteDir)
	base := filepath.Base(remoteDir)
	tarCmd := "tar cf -"
	if compress {
		tarCmd = "tar czf -"
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("%s -C %s %s", tarCmd, shellQuote(parent), shellQuote(base))}

	pr, pw := io.Pipe()
	var errBuf bytes.Buffer
	execErrCh := make(chan error, 1)
	go func() {
		err := c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, nil, pw, &errBuf, false)
		_ = pw.CloseWithError(err)
		execErrCh <- err
	}()

	var tarSource io.Reader = pr
	var gzReader *gzip.Reader
	if compress {
		gz, gzErr := gzip.NewReader(pr)
		if gzErr != nil {
			_ = pr.Close()
			<-execErrCh
			return false, fmt.Errorf("decompress remote tar: %w", gzErr)
		}
		gzReader = gz
		tarSource = gz
	}
	extractProduced, extractErr := extractTarToDir(tarSource, localDir, progress)
	if gzReader != nil {
		_ = gzReader.Close()
	}
	_ = pr.Close()
	execErr := <-execErrCh

	if extractErr != nil {
		return extractProduced, extractErr
	}
	if execErr != nil {
		msg := strings.TrimSpace(errBuf.String())
		if msg != "" {
			return extractProduced, fmt.Errorf("%w: %s", execErr, msg)
		}
		return extractProduced, execErr
	}
	return extractProduced, nil
}

func isRetryableCopyStreamError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	nonRetryable := []string{
		"not found",
		"no such file",
		"permission denied",
		"is a directory",
		"tar archive contains multiple files",
		"tar archive contained no file",
		"unsupported type",
	}
	for _, s := range nonRetryable {
		if strings.Contains(msg, s) {
			return false
		}
	}
	retryable := []string{
		"unexpected eof",
		"context deadline exceeded",
		"unexpected error when reading response body",
		"connection reset by peer",
		"timeout",
	}
	for _, s := range retryable {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// IsRemoteDir probes whether remotePath is a directory on the pod.
func (c *Client) IsRemoteDir(ctx context.Context, namespace, pod, container, remotePath string) (bool, error) {
	cs, cfg, err := c.clientset()
	if err != nil {
		return false, err
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("test -d %s", shellQuote(remotePath))}
	err = c.execStream(ctx, cs, cfg, namespace, pod, container, cmd, nil, io.Discard, io.Discard, false)
	if err == nil {
		return true, nil
	}
	// Non-zero exit from `test -d` means not a directory — not an error.
	return false, nil
}

func (c *Client) DescribePod(ctx context.Context, namespace, pod string) (string, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return "", err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	events, _ := cs.CoreV1().Events(namespace).List(ctx, metav1.ListOptions{FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", pod)})
	var b strings.Builder
	fmt.Fprintf(&b, "Pod: %s/%s\nPhase: %s\n", namespace, p.Name, p.Status.Phase)
	for _, status := range p.Status.ContainerStatuses {
		fmt.Fprintf(&b, "Container %s ready=%t restart=%d\n", status.Name, status.Ready, status.RestartCount)
	}
	if len(events.Items) > 0 {
		fmt.Fprintln(&b, "Events:")
		for _, ev := range events.Items {
			fmt.Fprintf(&b, "- %s: %s\n", ev.Reason, ev.Message)
		}
	}
	return b.String(), nil
}

// WatchPodEvents watches Kubernetes events for the given pod and calls onEvent
// for each relevant event. It blocks until the context is cancelled.
// Only events with reasons in the allowlist are forwarded.
func (c *Client) WatchPodEvents(ctx context.Context, namespace, pod string, onEvent func(reason, message string)) {
	cs, _, err := c.clientset()
	if err != nil {
		return
	}
	watcher, err := cs.CoreV1().Events(namespace).Watch(ctx, metav1.ListOptions{
		FieldSelector: fmt.Sprintf("involvedObject.kind=Pod,involvedObject.name=%s", pod),
	})
	if err != nil {
		return
	}
	defer watcher.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case evt, ok := <-watcher.ResultChan():
			if !ok {
				return
			}
			if evt.Type != watch.Added && evt.Type != watch.Modified {
				continue
			}
			event, ok := evt.Object.(*corev1.Event)
			if !ok {
				continue
			}
			if isPodEventRelevant(event.Reason) {
				onEvent(event.Reason, event.Message)
			}
		}
	}
}

func isPodEventRelevant(reason string) bool {
	switch reason {
	case "Pulling", "Pulled", "FailedScheduling", "BackOff", "Failed", "Unhealthy", "FailedMount", "FailedAttachVolume":
		return true
	}
	return false
}

func (c *Client) PodContainerNames(ctx context.Context, namespace, pod string) ([]string, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return nil, err
	}
	p, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}
	return podContainerNames(p), nil
}

func (c *Client) StreamPodLogs(ctx context.Context, namespace, pod string, opts LogStreamOptions, stdout io.Writer) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
	stream, err := cs.CoreV1().Pods(namespace).GetLogs(pod, buildPodLogOptions(opts)).Stream(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()
	_, err = io.Copy(stdout, stream)
	return err
}

func (c *Client) ExecSh(ctx context.Context, namespace, pod string, script string) ([]byte, error) {
	return c.execCapture(ctx, namespace, pod, "", []string{"sh", "-lc", script})
}

func (c *Client) ExecShInContainer(ctx context.Context, namespace, pod, container, script string) ([]byte, error) {
	return c.execCapture(ctx, namespace, pod, container, []string{"sh", "-lc", script})
}

func (c *Client) StreamShInContainer(ctx context.Context, namespace, pod, container, script string, stdout, stderr io.Writer) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	return c.execStream(ctx, cs, cfg, namespace, pod, container, []string{"sh", "-lc", script}, nil, stdout, stderr, false)
}

func (c *Client) execCapture(ctx context.Context, namespace, pod, container string, command []string) ([]byte, error) {
	cs, cfg, err := c.clientset()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer
	if err := c.execStream(ctx, cs, cfg, namespace, pod, container, command, nil, &out, &errBuf, false); err != nil {
		if container == "" {
			if fallback := preferredContainerFromExecErr(err); fallback != "" {
				out.Reset()
				errBuf.Reset()
				if retryErr := c.execStream(ctx, cs, cfg, namespace, pod, fallback, command, nil, &out, &errBuf, false); retryErr == nil {
					return out.Bytes(), nil
				}
			}
		}
		if errBuf.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func buildPodLogOptions(opts LogStreamOptions) *corev1.PodLogOptions {
	logOpts := &corev1.PodLogOptions{
		Container: strings.TrimSpace(opts.Container),
		Follow:    opts.Follow,
		Previous:  opts.Previous,
		TailLines: opts.TailLines,
	}
	if opts.Since > 0 {
		sinceSeconds := int64(opts.Since / time.Second)
		if sinceSeconds < 1 {
			sinceSeconds = 1
		}
		logOpts.SinceSeconds = &sinceSeconds
	}
	return logOpts
}

func podContainerNames(p *corev1.Pod) []string {
	if p == nil {
		return nil
	}
	names := make([]string, 0, len(p.Spec.Containers))
	for _, container := range p.Spec.Containers {
		name := strings.TrimSpace(container.Name)
		if name != "" {
			names = append(names, name)
		}
	}
	return names
}

func (c *Client) execStream(ctx context.Context, cs *kubernetes.Clientset, cfg *rest.Config, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error {
	req := cs.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod).
		Namespace(namespace).
		SubResource("exec")

	execOpts := &corev1.PodExecOptions{
		Command: command,
		Stdin:   stdin != nil,
		Stdout:  stdout != nil,
		Stderr:  stderr != nil,
		TTY:     tty,
	}
	if container != "" {
		execOpts.Container = container
	}
	req.VersionedParams(execOpts, scheme.ParameterCodec)
	exec, err := remotecommand.NewSPDYExecutor(cfg, http.MethodPost, req.URL())
	if err != nil {
		return err
	}
	return exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdin:  stdin,
		Stdout: stdout,
		Stderr: stderr,
		Tty:    tty,
	})
}

func (c *Client) portForwardURL(cs *kubernetes.Clientset, namespace, pod string) (*url.URL, error) {
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward")
	return req.URL(), nil
}

func shellQuote(s string) string {
	return shellutil.Quote(s)
}

// wrappers for testability.
var osWriteFile = func(path string, b []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, perm)
}

func podSummaryFromPod(p *corev1.Pod) PodSummary {
	readyContainers := 0
	totalContainers := len(p.Status.ContainerStatuses)
	var restarts int32
	reason := p.Status.Reason
	for _, st := range p.Status.ContainerStatuses {
		if st.Ready {
			readyContainers++
		}
		restarts += st.RestartCount
		if reason == "" && st.State.Waiting != nil && st.State.Waiting.Reason != "" {
			reason = st.State.Waiting.Reason
		}
	}
	if reason == "" {
		reason = "-"
	}
	deleting := p.DeletionTimestamp != nil
	phase := string(p.Status.Phase)
	if deleting {
		phase = "Terminating"
	}
	return PodSummary{
		Namespace:   p.Namespace,
		Name:        p.Name,
		Phase:       phase,
		Deleting:    deleting,
		CreatedAt:   p.CreationTimestamp.Time,
		Labels:      p.Labels,
		Annotations: p.Annotations,
		Ready:       fmt.Sprintf("%d/%d", readyContainers, totalContainers),
		Restarts:    restarts,
		Reason:      reason,
		PodIP:       p.Status.PodIP,
	}
}

func preferredContainerFromExecErr(err error) string {
	if err == nil {
		return ""
	}
	msg := err.Error()
	match := chooseContainerErrPattern.FindStringSubmatch(msg)
	if len(match) != 2 {
		return ""
	}
	names := strings.Fields(strings.TrimSpace(match[1]))
	if len(names) == 0 {
		return ""
	}
	for _, n := range names {
		if n == "dev" {
			return n
		}
	}
	for _, n := range names {
		if n != "okdev-sidecar" {
			return n
		}
	}
	return names[0]
}
