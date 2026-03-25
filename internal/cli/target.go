package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/output"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func newTargetCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "target",
		Short: "Show or change the pinned interactive target",
	}
	cmd.AddCommand(newTargetShowCmd(opts))
	cmd.AddCommand(newTargetSetCmd(opts))
	return cmd
}

func newTargetShowCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "show",
		Short: "Show the pinned target for the current session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg, ns)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			view, err := loadCurrentSessionView(cmd, opts, ns, sn)
			if err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), opts, cfg, ns, sn, k)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", view.Session)
			fmt.Fprintf(cmd.OutOrStdout(), "workload:  %s\n", view.WorkloadType)
			fmt.Fprintf(cmd.OutOrStdout(), "target:    %s\n", target.PodName)
			fmt.Fprintf(cmd.OutOrStdout(), "container: %s\n", target.Container)
			if strings.TrimSpace(target.Role) != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "role:      %s\n", target.Role)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "")
			rows := make([][]string, 0, len(view.Pods))
			for _, pod := range sortedSessionPods(view.Pods) {
				marker := ""
				if pod.Name == target.PodName {
					marker = "*"
				}
				rows = append(rows, []string{
					marker,
					pod.Name,
					strings.TrimSpace(pod.Labels["okdev.io/workload-role"]),
					strings.TrimSpace(pod.Labels["okdev.io/attachable"]),
					pod.Phase,
					pod.Ready,
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"SEL", "POD", "ROLE", "ATTACHABLE", "PHASE", "READY"}, rows)
			return nil
		},
	}
}

func newTargetSetCmd(opts *Options) *cobra.Command {
	var podName string
	var role string

	cmd := &cobra.Command{
		Use:   "set",
		Short: "Set the pinned interactive target",
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(podName) == "" && strings.TrimSpace(role) == "" {
				return fmt.Errorf("set one of --pod or --role")
			}
			if strings.TrimSpace(podName) != "" && strings.TrimSpace(role) != "" {
				return fmt.Errorf("--pod and --role cannot be used together")
			}
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg, ns)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			view, err := loadCurrentSessionView(cmd, opts, ns, sn)
			if err != nil {
				return err
			}
			targetPod, targetRole, err := resolveTargetCandidate(view, podName, role)
			if err != nil {
				return err
			}
			target := workload.TargetRef{
				PodName:   targetPod.Name,
				Container: resolveTargetContainer(cfg),
				Role:      targetRole,
			}
			if err := persistTargetRef(sn, target); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Pinned target for session %s: %s", sn, target.PodName)
			if target.Role != "" {
				fmt.Fprintf(cmd.OutOrStdout(), " (%s)", target.Role)
			}
			fmt.Fprintln(cmd.OutOrStdout())
			return nil
		},
	}
	cmd.Flags().StringVar(&podName, "pod", "", "Select a target pod by name")
	cmd.Flags().StringVar(&role, "role", "", "Select a target pod by workload role label")
	return cmd
}

func loadCurrentSessionView(cmd *cobra.Command, opts *Options, namespace, sessionName string) (sessionView, error) {
	label := "okdev.io/managed=true,okdev.io/session=" + sessionName
	pods, err := newKubeClient(opts).ListPods(cmd.Context(), namespace, false, label)
	if err != nil {
		return sessionView{}, err
	}
	if len(pods) == 0 {
		return sessionView{}, fmt.Errorf("no pods found for session %s", sessionName)
	}
	views := buildSessionViews(pods)
	if len(views) == 0 {
		return sessionView{}, fmt.Errorf("no session view found for session %s", sessionName)
	}
	return views[0], nil
}

func resolveTargetCandidate(view sessionView, podName, role string) (kube.PodSummary, string, error) {
	eligible := eligibleSessionTargetPods(view.Pods)
	if strings.TrimSpace(podName) != "" {
		for _, pod := range eligible {
			if pod.Name == podName {
				return pod, strings.TrimSpace(pod.Labels["okdev.io/workload-role"]), nil
			}
		}
		for _, pod := range view.Pods {
			if pod.Name == podName {
				return kube.PodSummary{}, "", fmt.Errorf("pod %q is not an attachable target in session %s", podName, view.Session)
			}
		}
		return kube.PodSummary{}, "", fmt.Errorf("pod %q not found in session %s", podName, view.Session)
	}
	role = strings.TrimSpace(role)
	if role == "" {
		return kube.PodSummary{}, "", fmt.Errorf("empty role")
	}
	candidates := make([]kube.PodSummary, 0)
	for _, pod := range eligible {
		if strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/workload-role"]), role) {
			candidates = append(candidates, pod)
		}
	}
	if len(candidates) == 0 {
		return kube.PodSummary{}, "", fmt.Errorf("no pods found for role %q in session %s", role, view.Session)
	}
	sort.Slice(candidates, func(i, j int) bool {
		return workload.ComparePodPriority(candidates[i], candidates[j])
	})
	chosen := candidates[0]
	return chosen, strings.TrimSpace(chosen.Labels["okdev.io/workload-role"]), nil
}

func sortedSessionPods(pods []kube.PodSummary) []kube.PodSummary {
	out := append([]kube.PodSummary(nil), pods...)
	sort.Slice(out, func(i, j int) bool {
		return workload.ComparePodPriority(out[i], out[j])
	})
	return out
}

func eligibleSessionTargetPods(pods []kube.PodSummary) []kube.PodSummary {
	attachable := make([]kube.PodSummary, 0, len(pods))
	for _, pod := range pods {
		if strings.EqualFold(strings.TrimSpace(pod.Labels["okdev.io/attachable"]), "true") {
			attachable = append(attachable, pod)
		}
	}
	if len(attachable) > 0 {
		return attachable
	}
	return append([]kube.PodSummary(nil), pods...)
}
