package kube

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/shellutil"
	"gopkg.in/yaml.v3"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/tools/remotecommand"
	"k8s.io/client-go/transport/spdy"
)

type Client struct {
	Context string

	mu           sync.Mutex
	cachedSet    *kubernetes.Clientset
	cachedConfig *rest.Config
}

type PodSummary struct {
	Namespace   string
	Name        string
	Phase       string
	CreatedAt   time.Time
	Labels      map[string]string
	Annotations map[string]string
}

type LeaseResult struct {
	Acquired      bool
	CurrentHolder string
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
	return restCfg, nil
}

func (c *Client) clientset() (*kubernetes.Clientset, *rest.Config, error) {
	c.mu.Lock()
	if c.cachedSet != nil && c.cachedConfig != nil {
		cs := c.cachedSet
		cfg := c.cachedConfig
		c.mu.Unlock()
		return cs, cfg, nil
	}
	c.mu.Unlock()

	restCfg, err := c.restConfig()
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create kubernetes client: %w", err)
	}

	c.mu.Lock()
	c.cachedSet = cs
	c.cachedConfig = restCfg
	c.mu.Unlock()

	return cs, restCfg, nil
}

func (c *Client) Apply(ctx context.Context, namespace string, manifest []byte) error {
	var tm metav1.TypeMeta
	if err := yaml.Unmarshal(manifest, &tm); err != nil {
		return fmt.Errorf("decode manifest type: %w", err)
	}
	cs, _, err := c.clientset()
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
		if reflect.DeepEqual(existing.Spec, pod.Spec) &&
			reflect.DeepEqual(existing.Labels, pod.Labels) &&
			reflect.DeepEqual(existing.Annotations, pod.Annotations) {
			return nil
		}
		if err := cs.CoreV1().Pods(namespace).Delete(ctx, pod.Name, metav1.DeleteOptions{}); err != nil {
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

		needsMetaUpdate := !reflect.DeepEqual(existing.Labels, pvc.Labels) || !reflect.DeepEqual(existing.Annotations, pvc.Annotations)
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
	default:
		return fmt.Errorf("unsupported manifest kind %q", tm.Kind)
	}
}

func pvcSpecDiff(existing, desired corev1.PersistentVolumeClaimSpec) (mutableChanged bool, immutableChanged bool) {
	if reflect.DeepEqual(existing, desired) {
		return false, false
	}
	mutableChanged = !reflect.DeepEqual(existing.Resources, desired.Resources) ||
		!reflect.DeepEqual(existing.VolumeAttributesClassName, desired.VolumeAttributesClassName)

	existingComparable := existing.DeepCopy()
	desiredComparable := desired.DeepCopy()
	existingComparable.Resources = desiredComparable.Resources
	existingComparable.VolumeAttributesClassName = desiredComparable.VolumeAttributesClassName
	if reflect.DeepEqual(existingComparable, desiredComparable) {
		return mutableChanged, false
	}
	return mutableChanged, true
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
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}

	var delErr error
	switch strings.ToLower(kind) {
	case "pod", "pods":
		delErr = cs.CoreV1().Pods(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "pvc", "persistentvolumeclaim", "persistentvolumeclaims":
		delErr = cs.CoreV1().PersistentVolumeClaims(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	case "lease", "leases":
		delErr = cs.CoordinationV1().Leases(namespace).Delete(ctx, name, metav1.DeleteOptions{})
	default:
		return fmt.Errorf("unsupported delete kind %q", kind)
	}
	if ignoreNotFound && apierrors.IsNotFound(delErr) {
		return nil
	}
	return delErr
}

func (c *Client) AcquireLease(ctx context.Context, namespace, name, holder, mode string, duration time.Duration) (LeaseResult, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return LeaseResult{}, err
	}
	now := metav1.NewMicroTime(time.Now().UTC())
	durationSec := int32(duration.Seconds())
	if durationSec <= 0 {
		durationSec = 120
	}

	leaseClient := cs.CoordinationV1().Leases(namespace)
	existing, err := leaseClient.Get(ctx, name, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		lease := &coordinationv1.Lease{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
			Spec: coordinationv1.LeaseSpec{
				HolderIdentity:       &holder,
				AcquireTime:          &now,
				RenewTime:            &now,
				LeaseDurationSeconds: &durationSec,
			},
		}
		if _, err := leaseClient.Create(ctx, lease, metav1.CreateOptions{}); err != nil {
			return LeaseResult{}, err
		}
		return LeaseResult{Acquired: true, CurrentHolder: holder}, nil
	}
	if err != nil {
		return LeaseResult{}, err
	}

	currentHolder := ""
	if existing.Spec.HolderIdentity != nil {
		currentHolder = *existing.Spec.HolderIdentity
	}

	expired := true
	if existing.Spec.RenewTime != nil && existing.Spec.LeaseDurationSeconds != nil {
		expiresAt := existing.Spec.RenewTime.Time.Add(time.Duration(*existing.Spec.LeaseDurationSeconds) * time.Second)
		expired = time.Now().After(expiresAt)
	}

	if currentHolder != "" && currentHolder != holder && !expired {
		if mode == "advisory" {
			return LeaseResult{Acquired: false, CurrentHolder: currentHolder}, nil
		}
		if mode == "exclusive" {
			return LeaseResult{}, fmt.Errorf("session lease held by %s", currentHolder)
		}
	}

	existing.Spec.HolderIdentity = &holder
	existing.Spec.RenewTime = &now
	existing.Spec.LeaseDurationSeconds = &durationSec
	if existing.Spec.AcquireTime == nil {
		existing.Spec.AcquireTime = &now
	}
	if _, err := leaseClient.Update(ctx, existing, metav1.UpdateOptions{}); err != nil {
		return LeaseResult{}, err
	}
	return LeaseResult{Acquired: true, CurrentHolder: holder}, nil
}

func (c *Client) WaitReady(ctx context.Context, namespace, pod string, timeout time.Duration) error {
	ctxWait, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cs, _, err := c.clientset()
	if err != nil {
		return err
	}

	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ctxWait.Done():
			return fmt.Errorf("wait for pod/%s ready: %w", pod, ctxWait.Err())
		case <-ticker.C:
			p, err := cs.CoreV1().Pods(namespace).Get(ctxWait, pod, metav1.GetOptions{})
			if err != nil {
				return err
			}
			if p.DeletionTimestamp != nil {
				return fmt.Errorf("pod/%s is terminating", pod)
			}
			for _, cond := range p.Status.Conditions {
				if cond.Type == corev1.PodReady && cond.Status == corev1.ConditionTrue {
					return nil
				}
			}
		}
	}
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
		out = append(out, PodSummary{
			Namespace:   p.Namespace,
			Name:        p.Name,
			Phase:       string(p.Status.Phase),
			CreatedAt:   p.CreationTimestamp.Time,
			Labels:      p.Labels,
			Annotations: p.Annotations,
		})
	}
	return out, nil
}

func (c *Client) TouchPodActivity(ctx context.Context, namespace, pod string) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
	current, err := cs.CoreV1().Pods(namespace).Get(ctx, pod, metav1.GetOptions{})
	if err != nil {
		return err
	}
	updated := current.DeepCopy()
	if updated.Annotations == nil {
		updated.Annotations = map[string]string{}
	}
	updated.Annotations["okdev.io/last-attach"] = time.Now().UTC().Format(time.RFC3339)
	_, err = cs.CoreV1().Pods(namespace).Update(ctx, updated, metav1.UpdateOptions{})
	return err
}

func (c *Client) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(command) == 0 {
		return errors.New("exec command cannot be empty")
	}
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	return c.execStream(ctx, cs, cfg, namespace, pod, "", command, stdin, stdout, stderr, tty)
}

func (c *Client) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
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
	dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, http.MethodPost, reqURL)

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

func (c *Client) CopyToPod(ctx context.Context, namespace, localPath, podName, remotePath string) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	b, err := osReadFile(localPath)
	if err != nil {
		return err
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("cat > %s", shellQuote(remotePath))}
	return c.execStream(ctx, cs, cfg, namespace, podName, "", cmd, bytes.NewReader(b), io.Discard, io.Discard, false)
}

func (c *Client) CopyFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	cs, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd := []string{"sh", "-lc", fmt.Sprintf("cat %s", shellQuote(remotePath))}
	if err := c.execStream(ctx, cs, cfg, namespace, podName, "", cmd, nil, &out, &errBuf, false); err != nil {
		return err
	}
	if err := osWriteFile(localPath, out.Bytes(), 0o644); err != nil {
		return err
	}
	return nil
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

func (c *Client) ExecSh(ctx context.Context, namespace, pod string, script string) ([]byte, error) {
	return c.execCapture(ctx, namespace, pod, "", []string{"sh", "-lc", script})
}

func (c *Client) ExecShInContainer(ctx context.Context, namespace, pod, container, script string) ([]byte, error) {
	return c.execCapture(ctx, namespace, pod, container, []string{"sh", "-lc", script})
}

func (c *Client) execCapture(ctx context.Context, namespace, pod, container string, command []string) ([]byte, error) {
	cs, cfg, err := c.clientset()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer
	if err := c.execStream(ctx, cs, cfg, namespace, pod, container, command, nil, &out, &errBuf, false); err != nil {
		if errBuf.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
		}
		return nil, err
	}
	return out.Bytes(), nil
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
var osReadFile = func(path string) ([]byte, error) { return os.ReadFile(path) }
var osWriteFile = func(path string, b []byte, perm fs.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, b, perm)
}
