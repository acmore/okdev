package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/output"
	"github.com/spf13/cobra"
)

func newStatusCmd(opts *Options) *cobra.Command {
	var all bool
	var allUsers bool
	var details bool

	cmd := &cobra.Command{
		Use:   "status [session]",
		Short: "Show session status",
		Example: `  # Show current session status
  okdev status

  # Show all sessions for this owner
  okdev status --all

  # Detailed diagnostics (SSH, sync, agents, pod events)
  okdev status --details

  # JSON output for scripting
  okdev status --output json`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, nil)
			if err != nil {
				return err
			}
			label := "okdev.io/managed=true"
			if !allUsers {
				label = label + "," + ownerLabelSelector(opts)
			}
			if !all {
				cc.sessionName, err = resolveSessionName(opts, cc.cfg, cc.namespace)
				if err != nil {
					return err
				}
				label = label + ",okdev.io/session=" + cc.sessionName
			}

			ctx, cancel := defaultContext()
			defer cancel()
			pods, err := cc.kube.ListPods(ctx, cc.namespace, false, label)
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching sessions found")
				return nil
			}
			views := buildSessionViews(pods)
			if details {
				if all || len(views) != 1 {
					return fmt.Errorf("--details requires a single session")
				}
				detail := gatherDetailedStatus(cmd.Context(), opts, cc.cfg, cc.cfgPath, cc.namespace, views[0], cc.kube)
				if opts.Output == "json" {
					return outputJSON(cmd.OutOrStdout(), detail)
				}
				printDetailedStatus(cmd.OutOrStdout(), detail)
				return nil
			}
			if opts.Output == "json" {
				type statusRow struct {
					Session   string `json:"session"`
					Owner     string `json:"owner"`
					Workload  string `json:"workload"`
					TargetPod string `json:"targetPod"`
					Phase     string `json:"phase"`
					Age       string `json:"age"`
					Ready     string `json:"ready"`
					Restarts  int32  `json:"restarts"`
					Reason    string `json:"reason"`
					PodCount  int    `json:"podCount"`
				}
				rows := make([]statusRow, 0, len(views))
				for _, view := range views {
					rows = append(rows, statusRow{
						Session:   view.Session,
						Owner:     view.Owner,
						Workload:  view.WorkloadType,
						TargetPod: view.TargetPod,
						Phase:     view.Phase,
						Age:       age(view.CreatedAt),
						Ready:     view.Ready,
						Restarts:  view.Restarts,
						Reason:    view.Reason,
						PodCount:  view.PodCount,
					})
				}
				return outputJSON(cmd.OutOrStdout(), rows)
			}

			rows := make([][]string, 0, len(views))
			for _, view := range views {
				rows = append(rows, []string{
					view.Session,
					view.Owner,
					view.WorkloadType,
					view.TargetPod,
					view.Phase,
					view.Ready,
					fmt.Sprintf("%d", view.PodCount),
					fmt.Sprintf("%d", view.Restarts),
					view.Reason,
					age(view.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"SESSION", "OWNER", "WORKLOAD", "TARGET", "PHASE", "READY", "PODS", "RESTARTS", "REASON", "AGE"}, rows)
			if !all && len(views) == 1 && len(views[0].Pods) > 1 {
				fmt.Fprintln(cmd.OutOrStdout(), "")
				podRows := make([][]string, 0, len(views[0].Pods))
				for _, pod := range views[0].Pods {
					marker := ""
					if pod.Name == views[0].TargetPod {
						marker = "*"
					}
					podRows = append(podRows, []string{
						marker,
						pod.Name,
						pod.Phase,
						pod.Ready,
						fmt.Sprintf("%d", pod.Restarts),
						pod.Reason,
						age(pod.CreatedAt),
					})
				}
				output.PrintTable(cmd.OutOrStdout(), []string{"SEL", "POD", "PHASE", "READY", "RESTARTS", "REASON", "AGE"}, podRows)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Show all sessions in namespace")
	cmd.Flags().BoolVar(&allUsers, "all-users", false, "Show sessions for all owners")
	cmd.Flags().BoolVar(&details, "details", false, "Show detailed diagnostics for a single session")
	return cmd
}
