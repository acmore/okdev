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
	"strings"
	"time"

	"gopkg.in/yaml.v3"
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
}

type PodSummary struct {
	Namespace string
	Name      string
	Phase     string
	CreatedAt time.Time
	Labels    map[string]string
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
	restCfg, err := c.restConfig()
	if err != nil {
		return nil, nil, err
	}
	cs, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, nil, fmt.Errorf("create kubernetes client: %w", err)
	}
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
		pod.ResourceVersion = existing.ResourceVersion
		_, err = cs.CoreV1().Pods(namespace).Update(ctx, &pod, metav1.UpdateOptions{})
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
		pvc.ResourceVersion = existing.ResourceVersion
		_, err = cs.CoreV1().PersistentVolumeClaims(namespace).Update(ctx, &pvc, metav1.UpdateOptions{})
		return err
	default:
		return fmt.Errorf("unsupported manifest kind %q", tm.Kind)
	}
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
	default:
		return fmt.Errorf("unsupported delete kind %q", kind)
	}
	if ignoreNotFound && apierrors.IsNotFound(delErr) {
		return nil
	}
	return delErr
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
			Namespace: p.Namespace,
			Name:      p.Name,
			Phase:     string(p.Status.Phase),
			CreatedAt: p.CreationTimestamp.Time,
			Labels:    p.Labels,
		})
	}
	return out, nil
}

func (c *Client) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if len(command) == 0 {
		return errors.New("exec command cannot be empty")
	}
	_, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	return c.execStream(ctx, cfg, namespace, pod, "", command, stdin, stdout, stderr, tty)
}

func (c *Client) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	_, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	reqURL, err := c.portForwardURL(namespace, pod)
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
	_, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	b, err := osReadFile(localPath)
	if err != nil {
		return err
	}
	cmd := []string{"sh", "-lc", fmt.Sprintf("cat > %s", shellQuote(remotePath))}
	return c.execStream(ctx, cfg, namespace, podName, "", cmd, bytes.NewReader(b), io.Discard, io.Discard, false)
}

func (c *Client) CopyFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	_, cfg, err := c.clientset()
	if err != nil {
		return err
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer
	cmd := []string{"sh", "-lc", fmt.Sprintf("cat %s", shellQuote(remotePath))}
	if err := c.execStream(ctx, cfg, namespace, podName, "", cmd, nil, &out, &errBuf, false); err != nil {
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
	_, cfg, err := c.clientset()
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	var errBuf bytes.Buffer
	if err := c.execStream(ctx, cfg, namespace, pod, container, command, nil, &out, &errBuf, false); err != nil {
		if errBuf.Len() > 0 {
			return nil, fmt.Errorf("%w: %s", err, strings.TrimSpace(errBuf.String()))
		}
		return nil, err
	}
	return out.Bytes(), nil
}

func (c *Client) execStream(ctx context.Context, cfg *rest.Config, namespace, pod, container string, command []string, stdin io.Reader, stdout, stderr io.Writer, tty bool) error {
	cs, _, err := c.clientset()
	if err != nil {
		return err
	}
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

func (c *Client) portForwardURL(namespace, pod string) (*url.URL, error) {
	cs, _, err := c.clientset()
	if err != nil {
		return nil, err
	}
	req := cs.CoreV1().RESTClient().Post().Resource("pods").Namespace(namespace).Name(pod).SubResource("portforward")
	return req.URL(), nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
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
