package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/output"
	"github.com/spf13/cobra"
)

func newStatusCmd(opts *Options) *cobra.Command {
	var all bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show session status",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			label := "okdev.io/managed=true"
			if !all {
				sn, err := resolveSessionName(opts, cfg)
				if err != nil {
					return err
				}
				label = label + ",okdev.io/session=" + sn
			}

			ctx, cancel := defaultContext()
			defer cancel()
			pods, err := newKubeClient(opts).ListPods(ctx, ns, false, label)
			if err != nil {
				return err
			}
			if len(pods) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No matching sessions found")
				return nil
			}

			rows := make([][]string, 0, len(pods))
			for _, p := range pods {
				rows = append(rows, []string{
					p.Labels["okdev.io/session"],
					p.Name,
					p.Phase,
					age(p.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"SESSION", "POD", "PHASE", "AGE"}, rows)
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "Show all sessions in namespace")
	return cmd
}
