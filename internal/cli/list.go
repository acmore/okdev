package cli

import (
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/output"
	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newListCmd(opts *Options) *cobra.Command {
	var allNamespaces bool
	var allUsers bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List dev sessions",
		RunE: func(cmd *cobra.Command, args []string) error {
			ns := opts.Namespace
			activeSession, activeErr := session.LoadActiveSession()
			if activeErr != nil {
				return activeErr
			}
			if ns == "" {
				if cfg, path, err := config.Load(opts.ConfigPath); err == nil && cfg.Spec.Namespace != "" {
					ns = cfg.Spec.Namespace
					announceConfigPath(path)
					applyConfigKubeContext(opts, cfg)
				} else if err == nil {
					announceConfigPath(path)
					applyConfigKubeContext(opts, cfg)
				}
			}
			if ns == "" {
				ns = "default"
			}
			ctx, cancel := defaultContext()
			defer cancel()
			label := "okdev.io/managed=true"
			if !allUsers {
				label = label + "," + ownerLabelSelector(opts)
			}
			pods, err := newKubeClient(opts).ListPods(ctx, ns, allNamespaces, label)
			if err != nil {
				return err
			}
			sort.Slice(pods, func(i, j int) bool {
				return pods[i].CreatedAt.After(pods[j].CreatedAt)
			})
			if opts.Output == "json" {
				type listRow struct {
					Namespace string `json:"namespace"`
					Session   string `json:"session"`
					Owner     string `json:"owner"`
					Phase     string `json:"phase"`
					Age       string `json:"age"`
					Active    bool   `json:"active"`
				}
				rows := make([]listRow, 0, len(pods))
				for _, p := range pods {
					sn := sessionNameFromPodSummary(p)
					rows = append(rows, listRow{
						Namespace: p.Namespace,
						Session:   sn,
						Owner:     p.Labels["okdev.io/owner"],
						Phase:     p.Phase,
						Age:       age(p.CreatedAt),
						Active:    sn != "" && sn == activeSession,
					})
				}
				return outputJSON(cmd.OutOrStdout(), rows)
			}
			rows := make([][]string, 0, len(pods))
			for _, p := range pods {
				sn := sessionNameFromPodSummary(p)
				if sn != "" && sn == activeSession {
					sn = "*" + sn
				}
				rows = append(rows, []string{
					p.Namespace,
					sn,
					p.Labels["okdev.io/owner"],
					p.Phase,
					age(p.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"NAMESPACE", "SESSION", "OWNER", "PHASE", "AGE"}, rows)
			return nil
		},
	}

	cmd.Flags().BoolVar(&allNamespaces, "all-namespaces", false, "List sessions across all namespaces")
	cmd.Flags().BoolVar(&allUsers, "all-users", false, "List sessions for all owners")
	return cmd
}

func sessionNameFromPodSummary(p kube.PodSummary) string {
	if p.Labels != nil {
		if sn := strings.TrimSpace(p.Labels["okdev.io/session"]); sn != "" {
			return sn
		}
	}
	name := strings.TrimSpace(p.Name)
	if strings.HasPrefix(name, "okdev-") {
		return strings.TrimPrefix(name, "okdev-")
	}
	return name
}
