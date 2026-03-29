package cli

import (
	"strconv"
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
		Example: `  # List your sessions in the current namespace
  okdev list

  # List across all namespaces
  okdev list --all-namespaces

  # List all users' sessions
  okdev list --all-users

  # JSON output
  okdev list --output json`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc := &commandContext{opts: opts}
			ns := opts.Namespace
			activeSession, activeErr := session.LoadActiveSession()
			if activeErr != nil {
				return activeErr
			}
			if ns == "" {
				if cfg, err := loadOptionalConfigForList(opts, announceConfigPath); err == nil && cfg.Spec.Namespace != "" {
					ns = cfg.Spec.Namespace
					applyConfigKubeContext(opts, cfg)
					cc.cfg = cfg
				} else if err == nil {
					applyConfigKubeContext(opts, cfg)
					cc.cfg = cfg
				}
			}
			if ns == "" {
				ns = "default"
			}
			cc.namespace = ns
			cc.kube = newKubeClient(opts)
			ctx, cancel := defaultContext()
			defer cancel()
			label := "okdev.io/managed=true"
			if !allUsers {
				label = label + "," + ownerLabelSelector(opts)
			}
			pods, err := cc.kube.ListPods(ctx, cc.namespace, allNamespaces, label)
			if err != nil {
				return err
			}
			views := buildSessionViews(pods)
			if opts.Output == "json" {
				type listRow struct {
					Namespace string `json:"namespace"`
					Session   string `json:"session"`
					Owner     string `json:"owner"`
					Workload  string `json:"workload"`
					TargetPod string `json:"targetPod"`
					Phase     string `json:"phase"`
					Pods      int    `json:"pods"`
					Age       string `json:"age"`
					Active    bool   `json:"active"`
				}
				rows := make([]listRow, 0, len(views))
				for _, view := range views {
					rows = append(rows, listRow{
						Namespace: view.Namespace,
						Session:   view.Session,
						Owner:     view.Owner,
						Workload:  view.WorkloadType,
						TargetPod: view.TargetPod,
						Phase:     view.Phase,
						Pods:      view.PodCount,
						Age:       age(view.CreatedAt),
						Active:    view.Session != "" && view.Session == activeSession,
					})
				}
				return outputJSON(cmd.OutOrStdout(), rows)
			}
			rows := make([][]string, 0, len(views))
			for _, view := range views {
				sn := view.Session
				if sn != "" && sn == activeSession {
					sn = "*" + sn
				}
				rows = append(rows, []string{
					view.Namespace,
					sn,
					view.Owner,
					view.WorkloadType,
					view.TargetPod,
					view.Phase,
					strconv.Itoa(view.PodCount),
					age(view.CreatedAt),
				})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"NAMESPACE", "SESSION", "OWNER", "WORKLOAD", "TARGET", "PHASE", "PODS", "AGE"}, rows)
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

func loadOptionalConfigForList(opts *Options, announce func(string) func(bool)) (*config.DevEnvironment, error) {
	path, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return nil, err
	}
	done := announce(path)
	cfg, _, err := config.Load(path)
	done(err == nil)
	if err != nil {
		return nil, err
	}
	return cfg, nil
}
