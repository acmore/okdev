package cli

import (
	"fmt"
	"time"

	"github.com/spf13/cobra"
)

func newPruneCmd(opts *Options) *cobra.Command {
	var allNamespaces bool
	var ttlHours int

	cmd := &cobra.Command{
		Use:   "prune",
		Short: "Delete expired sessions by TTL",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			if ttlHours <= 0 {
				ttlHours = cfg.Spec.Session.TTLHours
			}
			if ttlHours <= 0 {
				ttlHours = 72
			}

			ctx, cancel := defaultContext()
			defer cancel()
			k := newKubeClient(opts)
			pods, err := k.ListPods(ctx, ns, allNamespaces, "okdev.io/managed=true")
			if err != nil {
				return err
			}

			now := time.Now()
			deleted := 0
			for _, p := range pods {
				if p.CreatedAt.IsZero() {
					continue
				}
				if now.Sub(p.CreatedAt) < time.Duration(ttlHours)*time.Hour {
					continue
				}
				sessionName := p.Labels["okdev.io/session"]
				if sessionName == "" {
					sessionName = p.Name
				}
				if err := k.Delete(ctx, p.Namespace, "pod", p.Name, true); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Pruned session %s in namespace %s\n", sessionName, p.Namespace)
				deleted++
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Prune complete, deleted %d session(s)\n", deleted)
			return nil
		},
	}

	cmd.Flags().BoolVar(&allNamespaces, "all-namespaces", false, "Prune sessions across all namespaces")
	cmd.Flags().IntVar(&ttlHours, "ttl-hours", 0, "TTL in hours override")
	return cmd
}
