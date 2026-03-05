package kube

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"time"
)

type Client struct {
	Context string
}

func (c *Client) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	args := make([]string, 0, 16+len(command))
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	args = append(args, "-n", namespace, "exec")
	if tty {
		args = append(args, "-it")
	} else {
		args = append(args, "-i")
	}
	args = append(args, pod, "--")
	args = append(args, command...)

	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdin = stdin
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (c *Client) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	args := make([]string, 0, 8+len(forwards))
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	args = append(args, "-n", namespace, "port-forward", "pod/"+pod)
	args = append(args, forwards...)
	cmd := exec.CommandContext(ctx, "kubectl", args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("kubectl %s: %w", strings.Join(args, " "), err)
	}
	return nil
}

func (c *Client) CopyToPod(ctx context.Context, namespace, localPath, podName, remotePath string) error {
	args := make([]string, 0, 8)
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	args = append(args, "-n", namespace, "cp", localPath, podName+":"+remotePath)
	_, err := c.Kubectl(ctx, nil, args...)
	return err
}

func (c *Client) CopyFromPod(ctx context.Context, namespace, podName, remotePath, localPath string) error {
	args := make([]string, 0, 8)
	if c.Context != "" {
		args = append(args, "--context", c.Context)
	}
	args = append(args, "-n", namespace, "cp", podName+":"+remotePath, localPath)
	_, err := c.Kubectl(ctx, nil, args...)
	return err
}

func (c *Client) DescribePod(ctx context.Context, namespace, pod string) (string, error) {
	out, err := c.Kubectl(ctx, nil, "-n", namespace, "describe", "pod", pod)
	if err != nil {
		return "", err
	}
	return string(out), nil
}

func (c *Client) ExecSh(ctx context.Context, namespace, pod string, script string) ([]byte, error) {
	args := []string{"-n", namespace, "exec", "-i", pod, "--", "sh", "-lc", script}
	return c.Kubectl(ctx, nil, args...)
}

type PodSummary struct {
	Namespace string
	Name      string
	Phase     string
	CreatedAt time.Time
	Labels    map[string]string
}

func (c *Client) Kubectl(ctx context.Context, stdin []byte, args ...string) ([]byte, error) {
	full := make([]string, 0, len(args)+2)
	if c.Context != "" {
		full = append(full, "--context", c.Context)
	}
	full = append(full, args...)

	cmd := exec.CommandContext(ctx, "kubectl", full...)
	if len(stdin) > 0 {
		cmd.Stdin = bytes.NewReader(stdin)
	}
	var out bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("kubectl %s: %s", strings.Join(full, " "), msg)
	}
	return out.Bytes(), nil
}

func (c *Client) Apply(ctx context.Context, namespace string, manifest []byte) error {
	_, err := c.Kubectl(ctx, manifest, "-n", namespace, "apply", "-f", "-")
	return err
}

func (c *Client) Delete(ctx context.Context, namespace string, kind string, name string, ignoreNotFound bool) error {
	args := []string{"-n", namespace, "delete", kind, name}
	if ignoreNotFound {
		args = append(args, "--ignore-not-found=true")
	}
	_, err := c.Kubectl(ctx, nil, args...)
	return err
}

func (c *Client) WaitReady(ctx context.Context, namespace, pod string, timeout time.Duration) error {
	_, err := c.Kubectl(ctx, nil, "-n", namespace, "wait", "--for=condition=Ready", "pod/"+pod, "--timeout", timeout.String())
	return err
}

func (c *Client) ListPods(ctx context.Context, namespace string, allNamespaces bool, labelSelector string) ([]PodSummary, error) {
	args := []string{"get", "pods", "-l", labelSelector, "-o", "json"}
	if allNamespaces {
		args = append(args, "-A")
	} else {
		args = append(args, "-n", namespace)
	}
	out, err := c.Kubectl(ctx, nil, args...)
	if err != nil {
		return nil, err
	}

	var payload struct {
		Items []struct {
			Metadata struct {
				Name              string            `json:"name"`
				Namespace         string            `json:"namespace"`
				CreationTimestamp time.Time         `json:"creationTimestamp"`
				Labels            map[string]string `json:"labels"`
			} `json:"metadata"`
			Status struct {
				Phase string `json:"phase"`
			} `json:"status"`
		} `json:"items"`
	}
	if err := json.Unmarshal(out, &payload); err != nil {
		return nil, fmt.Errorf("parse kubectl json: %w", err)
	}

	res := make([]PodSummary, 0, len(payload.Items))
	for _, it := range payload.Items {
		res = append(res, PodSummary{
			Namespace: it.Metadata.Namespace,
			Name:      it.Metadata.Name,
			Phase:     it.Status.Phase,
			CreatedAt: it.Metadata.CreationTimestamp,
			Labels:    it.Metadata.Labels,
		})
	}
	return res, nil
}
