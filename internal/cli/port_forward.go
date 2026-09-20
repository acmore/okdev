package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func newPortForwardCmd(opts *Options) *cobra.Command {
	var podName string
	var role string
	var readyOnly bool
	var addresses []string

	cmd := &cobra.Command{
		Use:   "port-forward [session] <local:remote>...",
		Short: "Run a direct foreground pod port-forward",
		Args: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(podName) != "" && strings.TrimSpace(role) != "" {
				return fmt.Errorf("--pod and --role are mutually exclusive")
			}
			_, mappingArgs := splitPortForwardArgs(args)
			_, err := parsePortForwardMappings(mappingArgs)
			return err
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionArgs, mappingArgs := splitPortForwardArgs(args)
			applySessionArg(opts, sessionArgs)
			cc, err := resolveCommandContext(opts, nil)
			if err != nil {
				return err
			}
			mappings, err := parsePortForwardMappings(mappingArgs)
			if err != nil {
				return err
			}
			listenAddresses, err := normalizePortForwardAddresses(addresses)
			if err != nil {
				return err
			}

			resolve := func(ctx context.Context) (workload.TargetRef, error) {
				if cc.sessionName == "" {
					name, err := resolveSessionNameWithReader(cc.opts, cc.cfg, cc.namespace, true, cc.kube, ctx)
					if err != nil {
						return workload.TargetRef{}, err
					}
					cc.sessionName = name
					if err := selectCommandWorkload(cc); err != nil {
						return workload.TargetRef{}, err
					}
				}
				if err := ensureSessionAccess(cc.opts, cc.kube, cc.namespace, cc.sessionName, true, ctx); err != nil {
					return workload.TargetRef{}, err
				}
				return resolvePortForwardTarget(ctx, cc, podName, role, readyOnly)
			}
			forward := func(ctx context.Context, target workload.TargetRef) error {
				fmt.Fprintf(cmd.OutOrStdout(), "Forwarding to pod=%s %s\n", target.PodName, strings.Join(mappings, ", "))
				return forwardWhilePodExists(ctx, cc.kube, cc.namespace, target.PodName, 5*time.Second, func(ctx context.Context) error {
					return cc.kube.PortForwardOnceOnAddresses(ctx, cc.namespace, target.PodName, listenAddresses, mappings, cmd.OutOrStdout(), cmd.ErrOrStderr())
				})
			}
			return recoverPortForward(cmd.Context(), resolve, forward, podName == "", time.Second, cmd.ErrOrStderr())
		},
	}
	cmd.Flags().StringVar(&podName, "pod", "", "Target a specific pod by name")
	cmd.Flags().StringVar(&role, "role", "", "Target a pod by workload role")
	cmd.Flags().StringSliceVar(&addresses, "address", []string{"localhost"}, "Addresses to listen on (repeatable or comma-separated)")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Use only already-running pods")
	return cmd
}

func resolvePortForwardTarget(ctx context.Context, cc *commandContext, podName, role string, readyOnly bool) (workload.TargetRef, error) {
	var podNames []string
	switch {
	case strings.TrimSpace(podName) != "":
		podNames = []string{podName}
	case strings.TrimSpace(role) == "":
		pinned, err := resolveTargetRef(ctx, cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
		if err != nil {
			return workload.TargetRef{}, err
		}
		podNames = []string{pinned.PodName}
	}
	pods, err := selectSessionPods(ctx, cc, podNames, role, nil, nil, readyOnly)
	if err != nil {
		return workload.TargetRef{}, err
	}
	pod, err := selectSinglePortForwardPod(pods)
	if err != nil {
		return workload.TargetRef{}, err
	}
	return workload.TargetRef{PodName: pod.Name}, nil
}

func parsePortForwardMappings(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("requires at least one LOCAL:REMOTE mapping")
	}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		parts := strings.Split(arg, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port mapping %q, expected LOCAL:REMOTE", arg)
		}
		local, err := strconv.Atoi(parts[0])
		if err != nil || local <= 0 || local > 65535 {
			return nil, fmt.Errorf("invalid local port in mapping %q", arg)
		}
		remote, err := strconv.Atoi(parts[1])
		if err != nil || remote <= 0 || remote > 65535 {
			return nil, fmt.Errorf("invalid remote port in mapping %q", arg)
		}
		out = append(out, fmt.Sprintf("%d:%d", local, remote))
	}
	return out, nil
}

func recoverPortForward(ctx context.Context, resolve func(context.Context) (workload.TargetRef, error), forward func(context.Context, workload.TargetRef) error, follow bool, initialBackoff time.Duration, errOut io.Writer) error {
	backoff := initialBackoff
	selected := false
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		start := time.Now()
		target, err := resolve(ctx)
		resolving := err != nil
		if err == nil {
			selected = true
			err = forward(ctx, target)
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !retryForegroundForward(err, resolving, selected && follow) {
			return err
		}
		if time.Since(start) > time.Minute {
			backoff = initialBackoff
		}
		fmt.Fprintf(errOut, "port-forward unavailable: %v; retrying in %s (local listeners are closed during reconnect)\n", err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		backoff = min(backoff*2, 30*time.Second)
	}
}

func forwardWhilePodExists(ctx context.Context, client kubeClientTargetGetter, namespace, pod string, interval time.Duration, forward func(context.Context) error) error {
	ctx, cancel := context.WithCancelCause(ctx)
	defer cancel(context.Canceled)
	get := func() (*kube.PodSummary, error) {
		checkCtx, checkCancel := context.WithTimeout(ctx, 5*time.Second)
		defer checkCancel()
		return client.GetPodSummary(checkCtx, namespace, pod)
	}
	initial, err := get()
	if err != nil {
		return err
	}
	missing := func() error { return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod) }
	if initial.Deleting {
		return missing()
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				summary, err := get()
				if err == nil && (summary.Deleting || summary.UID != initial.UID) {
					err = missing()
				}
				if err != nil && !retryForegroundForward(err, true, false) {
					cancel(err)
					return
				}
			}
		}
	}()
	err = forward(ctx)
	cancel(context.Canceled)
	<-done
	if cause := context.Cause(ctx); !errors.Is(cause, context.Canceled) {
		return cause
	}
	return err
}

func retryForegroundForward(err error, resolving, follow bool) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) || apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) || apierrors.IsBadRequest(err) || apierrors.IsInvalid(err) {
		return false
	}
	message := strings.ToLower(err.Error())
	for _, terminal := range []string{"forbidden", "unauthorized", "unable to listen", "address already in use", "cannot assign requested address", "certificate", "invalid", "bad request"} {
		if strings.Contains(message, terminal) {
			// A DNS name can contain words such as "invalid" without making
			// a DNS lookup failure a terminal configuration error.
			var dns *net.DNSError
			if !errors.As(err, &dns) {
				return false
			}
		}
	}
	var dns *net.DNSError
	if errors.As(err, &dns) || isTransientClusterError(err) || errors.Is(err, ErrTransientCluster) || errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) {
		return true
	}
	if apierrors.IsNotFound(err) || errors.Is(err, ErrSessionNotFound) || strings.Contains(message, "not found") {
		return follow
	}
	if resolving {
		if !follow {
			return false
		}
		for _, unavailable := range []string{"no workload pods", "no attachable pods", "no session pod matches", "no pods match", "no running pods", "pods are not running"} {
			if strings.Contains(message, unavailable) {
				return true
			}
		}
		return false
	}
	return strings.Contains(message, "lost connection to pod") || strings.Contains(message, "broken pipe") || strings.Contains(message, "error dialing backend")
}

func normalizePortForwardAddresses(values []string) ([]string, error) {
	if len(values) == 0 {
		return []string{"localhost"}, nil
	}
	out := make([]string, 0, len(values))
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		for _, part := range strings.Split(value, ",") {
			addr := strings.TrimSpace(part)
			if addr == "" {
				return nil, fmt.Errorf("invalid --address value %q", value)
			}
			if _, ok := seen[addr]; ok {
				continue
			}
			seen[addr] = struct{}{}
			out = append(out, addr)
		}
	}
	if len(out) == 0 {
		return []string{"localhost"}, nil
	}
	return out, nil
}

func selectSinglePortForwardPod(pods []kube.PodSummary) (kube.PodSummary, error) {
	if len(pods) != 1 {
		return kube.PodSummary{}, fmt.Errorf("port-forward requires exactly one pod, got %d", len(pods))
	}
	return pods[0], nil
}

// splitPortForwardArgs separates an optional leading session name from the
// LOCAL:REMOTE mapping args. The first arg is only treated as a session name
// when it lacks a colon AND at least one more arg follows; otherwise it is
// left in the mapping list so the parser surfaces a clear error.
func splitPortForwardArgs(args []string) ([]string, []string) {
	if len(args) >= 2 && !strings.Contains(args[0], ":") {
		return args[:1], args[1:]
	}
	return nil, args
}

type portForwardRunner interface {
	PortForwardOnAddresses(context.Context, string, string, []string, []string, io.Writer, io.Writer) error
}

func runPortForward(ctx context.Context, client portForwardRunner, namespace string, target workload.TargetRef, addresses []string, forwards []string, out io.Writer) error {
	fmt.Fprintf(out, "Forwarding to pod=%s %s\n", target.PodName, strings.Join(forwards, ", "))
	return client.PortForwardOnAddresses(ctx, namespace, target.PodName, addresses, forwards, out, io.Discard)
}
