package cli

import (
	"context"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
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
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
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

			target, err := resolvePortForwardTarget(cmd.Context(), cc, podName, role, readyOnly)
			if err != nil {
				return err
			}
			return runPortForward(cmd.Context(), cc.kube, cc.namespace, target, listenAddresses, mappings, cmd.OutOrStdout())
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
		if err != nil || local <= 0 {
			return nil, fmt.Errorf("invalid local port in mapping %q", arg)
		}
		remote, err := strconv.Atoi(parts[1])
		if err != nil || remote <= 0 {
			return nil, fmt.Errorf("invalid remote port in mapping %q", arg)
		}
		out = append(out, fmt.Sprintf("%d:%d", local, remote))
	}
	return out, nil
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
