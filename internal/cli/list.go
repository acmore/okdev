package cli

import (
	"sort"

	"github.com/acmore/okdev/internal/output"
	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	var allNamespaces bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List dev sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			_ = cfg
			ctx, cancel := defaultContext()
			defer cancel()
			pods, err := newKubeClient(opts).ListPods(ctx, ns, allNamespaces, "okdev.io/managed=true")
			if err != nil {
				return err
			}
			sort.Slice(pods, func(i, j int) bool {
				return pods[i].CreatedAt.After(pods[j].CreatedAt)
			})
			rows := make([][]string, 0, len(pods))
			for _, p := range pods {
				rows = append(rows, []string{
					p.Namespace,
					p.Labels["okdev.io/session"],
					p.Phase,
					age(p.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"NAMESPACE", "SESSION", "PHASE", "AGE"}, rows)
			return nil
		},
	}

	cmd.Flags().BoolVar(&allNamespaces, "all-namespaces", false, "List sessions across all namespaces")
	return cmd
}
