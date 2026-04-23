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
	var container string
	var readyOnly bool

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

			var target workload.TargetRef
			if strings.TrimSpace(podName) == "" && strings.TrimSpace(role) == "" {
				target, err = resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
				if err != nil {
					return err
				}
			} else {
				var podNames []string
				if strings.TrimSpace(podName) != "" {
					podNames = []string{podName}
				}
				pods, err := selectSessionPods(cmd.Context(), cc, podNames, role, nil, nil, readyOnly)
				if err != nil {
					return err
				}
				pod, err := selectSinglePortForwardPod(pods, "", "")
				if err != nil {
					return err
				}
				target = workload.TargetRef{
					PodName:   pod.Name,
					Container: resolveTargetContainer(cc.cfg),
				}
			}
			if strings.TrimSpace(container) != "" {
				target.Container = container
			}
			return runPortForward(cmd.Context(), cc.kube, cc.namespace, target, mappings, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&podName, "pod", "", "Target a specific pod by name")
	cmd.Flags().StringVar(&role, "role", "", "Target a pod by workload role")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Use only already-running pods")
	return cmd
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

func selectSinglePortForwardPod(pods []kube.PodSummary, podName, role string) (kube.PodSummary, error) {
	candidates := pods
	switch {
	case strings.TrimSpace(podName) != "":
		candidates = filterPodsByName(candidates, []string{podName})
	case strings.TrimSpace(role) != "":
		candidates = filterPodsByRole(candidates, role)
	}
	if len(candidates) != 1 {
		return kube.PodSummary{}, fmt.Errorf("port-forward requires exactly one pod, got %d", len(candidates))
	}
	return candidates[0], nil
}

func splitPortForwardArgs(args []string) ([]string, []string) {
	if len(args) > 0 && !strings.Contains(args[0], ":") {
		return args[:1], args[1:]
	}
	return nil, args
}

type portForwardRunner interface {
	PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}

func runPortForward(ctx context.Context, client portForwardRunner, namespace string, target workload.TargetRef, forwards []string, out io.Writer) error {
	fmt.Fprintf(out, "Forwarding session target pod=%s container=%s %s\n", target.PodName, target.Container, strings.Join(forwards, ", "))
	return client.PortForward(ctx, namespace, target.PodName, forwards, out, io.Discard)
}
