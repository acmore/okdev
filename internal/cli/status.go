package cli

import (
	"fmt"
	"sort"

	"github.com/acmore/okdev/internal/output"
	"github.com/spf13/cobra"
)

func newStatusCmd(opts *Options) *cobra.Command {
	var all bool
	var allUsers bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show session status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			label := "okdev.io/managed=true"
			if !allUsers {
				label = label + "," + ownerLabelSelector(opts)
			}
			if !all {
				sn, err := resolveSessionName(opts, cfg)
				if err != nil {
					return err
				}
				label = label + ",okdev.io/session=" + sn
			}

			ctx, cancel := defaultContext()
			defer cancel()
			k := newKubeClient(opts)
			pods, err := k.ListPods(ctx, ns, false, label)
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching sessions found")
				return nil
			}
			sort.Slice(pods, func(i, j int) bool {
				return pods[i].CreatedAt.After(pods[j].CreatedAt)
			})
			if opts.Output == "json" {
				type statusRow struct {
					Session    string `json:"session"`
					Owner      string `json:"owner"`
					Pod        string `json:"pod"`
					Phase      string `json:"phase"`
					Age        string `json:"age"`
					Ready      string `json:"ready"`
					Restarts   int32  `json:"restarts"`
					Reason     string `json:"reason"`
					LockHolder string `json:"lockHolder,omitempty"`
				}
				rows := make([]statusRow, 0, len(pods))
				for _, p := range pods {
					sn := p.Labels["okdev.io/session"]
					lockHolder, err := k.LeaseHolder(ctx, p.Namespace, "okdev-"+sn)
					if err != nil {
						return err
					}
					rows = append(rows, statusRow{
						Session:    sn,
						Owner:      p.Labels["okdev.io/owner"],
						Pod:        p.Name,
						Phase:      p.Phase,
						Age:        age(p.CreatedAt),
						Ready:      p.Ready,
						Restarts:   p.Restarts,
						Reason:     p.Reason,
						LockHolder: lockHolder,
					})
				}
				return outputJSON(cmd.OutOrStdout(), rows)
			}

			rows := make([][]string, 0, len(pods))
			for _, p := range pods {
				sn := p.Labels["okdev.io/session"]
				lockHolder, err := k.LeaseHolder(ctx, p.Namespace, "okdev-"+sn)
				if err != nil {
					return err
				}
				if lockHolder == "" {
					lockHolder = "-"
				}
				rows = append(rows, []string{
					sn,
					p.Labels["okdev.io/owner"],
					p.Name,
					p.Phase,
					p.Ready,
					fmt.Sprintf("%d", p.Restarts),
					p.Reason,
					lockHolder,
					age(p.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"SESSION", "OWNER", "POD", "PHASE", "READY", "RESTARTS", "REASON", "LOCK", "AGE"}, rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Show all sessions in namespace")
	cmd.Flags().BoolVar(&allUsers, "all-users", false, "Show sessions for all owners")
	return cmd
}
