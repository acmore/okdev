package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func allowAttachCommand(name string) error {
	switch name {
	case "exec", "jobs", "exec-jobs", "cp", "attach-setup", "validate":
		return nil
	default:
		return fmt.Errorf("%s is unavailable in attach-only mode; use exec, jobs, cp or explicit attach-setup", name)
	}
}

func listCommandPods(ctx context.Context, cc *commandContext) ([]kube.PodSummary, error) {
	if cc.cfg.Spec.AttachOnly != nil {
		return listAttachPods(ctx, cc.opts, cc.kube, cc.namespace)
	}
	return cc.kube.ListPods(ctx, cc.namespace, false, selectorForSessionRun(cc.sessionName))
}

func listAttachPods(ctx context.Context, opts *Options, k targetResolverClient, namespace string) ([]kube.PodSummary, error) {
	a := opts.attachOnly
	var pods []kube.PodSummary
	if len(a.Pods) > 0 {
		seen := map[string]bool{}
		for _, name := range a.Pods {
			if seen[name] {
				continue
			}
			seen[name] = true
			pod, err := k.GetPodSummary(ctx, namespace, name)
			if err != nil {
				return nil, err
			}
			if pod == nil {
				return nil, fmt.Errorf("attach-only pod %q not found", name)
			}
			pods = append(pods, *pod)
		}
	} else {
		selector := a.Selector
		if a.Session != "" {
			selector = "okdev.io/managed=true,okdev.io/session=" + a.Session
		}
		var err error
		pods, err = k.ListPods(ctx, namespace, false, selector)
		if err != nil {
			return nil, err
		}
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("attach-only scope matches no existing pods in namespace %q", namespace)
	}
	for _, pod := range pods {
		if owner := strings.TrimSpace(pod.Labels["okdev.io/owner"]); owner != "" && owner != currentOwner(opts) {
			return nil, fmt.Errorf("attach-only pod %q is owned by %q (current owner: %q); set --owner %s", pod.Name, owner, currentOwner(opts), owner)
		}
	}
	sort.Slice(pods, func(i, j int) bool { return pods[i].Name < pods[j].Name })
	return pods, nil
}

func resolveAttachTarget(ctx context.Context, opts *Options, k targetResolverClient, namespace string) (workload.TargetRef, error) {
	pods, err := listAttachPods(ctx, opts, k, namespace)
	if err != nil {
		return workload.TargetRef{}, err
	}
	if len(pods) != 1 {
		return workload.TargetRef{}, fmt.Errorf("attach-only scope has %d pods; select an explicit --pod or --all", len(pods))
	}
	return workload.TargetRef{PodName: pods[0].Name, Container: opts.attachOnly.Container}, nil
}

func newAttachSetupCmd(opts *Options) *cobra.Command {
	var pods []string
	var all bool
	cmd := &cobra.Command{
		Use: "attach-setup", Short: "Explicitly run attach-only setup on existing pods",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if cc.cfg.Spec.AttachOnly == nil {
				return fmt.Errorf("attach-setup requires spec.attachOnly")
			}
			script := cc.cfg.Spec.AttachOnly.Setup
			if strings.TrimSpace(script) == "" {
				return fmt.Errorf("spec.attachOnly.setup is not configured")
			}
			if !all && len(pods) == 0 {
				target, err := resolveAttachTarget(cmd.Context(), cc.opts, cc.kube, cc.namespace)
				if err != nil {
					return err
				}
				pods = []string{target.PodName}
			}
			return runMultiPodExec(cmd, cc, execInvocation{Argv: []string{"sh", "-c", script}, DisplayCommand: script}, execTargetPlan{podNames: pods, allPods: all}, cc.cfg.Spec.AttachOnly.Container, false, false, 0, "", false, 1, false, "", true)
		},
	}
	cmd.Flags().StringSliceVar(&pods, "pod", nil, "Select pods within the attach-only scope")
	cmd.Flags().BoolVar(&all, "all", false, "Run setup on all pods in the attach-only scope")
	cmd.MarkFlagsMutuallyExclusive("pod", "all")
	return cmd
}
