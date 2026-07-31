package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/output"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func newWorkloadCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "workload",
		Short: "Inspect and switch the workload this session runs",
		Long: `Switch what the current session runs, without creating a new session.

  okdev use <session>         switches WHICH SESSION commands target
  okdev workload use <name>   switches WHAT the current session runs

Switching replaces the running workload: the session name, sync channel, ports
and SSH alias all stay, and the old workload is deleted. To run two shapes at
the same time, use two sessions instead (okdev up --session other).

This group inspects and switches; to declare a new workload, use
okdev init --workload <type> --workload-name <name>.`,
		Example: `  # See what this config declares, and what is pinned and live
  okdev workload list

  # Declare a new workload and scaffold its manifest
  okdev init --workload pytorchjob --workload-name train

  # Switch this session to it (applies on the next okdev up)
  okdev workload use train
  okdev up`,
	}
	cmd.AddCommand(newWorkloadListCmd(opts))
	cmd.AddCommand(newWorkloadUseCmd(opts))
	cmd.AddCommand(newWorkloadShowCmd(opts))
	return cmd
}

func newWorkloadListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "List the workloads this config declares",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			pinned, err := session.LoadWorkloadProfile(cc.sessionName)
			if err != nil {
				return err
			}
			live := liveWorkloadProfile(cmd.Context(), cc)

			rows := make([][]string, 0, len(cc.cfg.Spec.Workloads))
			for _, p := range cc.cfg.Spec.Workloads {
				rows = append(rows, []string{
					p.Name,
					normalizeWorkloadType(p.Type),
					manifestCell(p),
					markCell(p.Name == pinned),
					markCell(p.Name == live),
				})
			}
			output.PrintTable(cmd.OutOrStdout(),
				[]string{"NAME", "TYPE", "MANIFEST", "PINNED", "LIVE"}, rows)
			return nil
		},
	}
}

func manifestCell(p config.WorkloadProfile) string {
	if m := strings.TrimSpace(p.ManifestPath); m != "" {
		return m
	}
	return "(spec.podTemplate)"
}

func markCell(on bool) string {
	if on {
		return "*"
	}
	return "-"
}

// liveWorkloadProfile reads the profile label off the session's running pods.
// A cluster error is not fatal here: `workload list` must still show the
// declared profiles and the pin when the cluster is unreachable.
func liveWorkloadProfile(ctx context.Context, cc *commandContext) string {
	if cc == nil || cc.kube == nil {
		return ""
	}
	pods, err := cc.kube.ListPods(ctx, cc.namespace, false,
		"okdev.io/managed=true,okdev.io/session="+cc.sessionName)
	if err != nil {
		return ""
	}
	return liveProfileFromPods(cc.cfg, pods)
}

// liveProfileFromPods names the profile the session's pods belong to.
//
// Pods created before the profile label existed carry none. Falling back to the
// workload type — the same signal detectWorkloadSwitch uses for those pods —
// keeps `workload list` from showing PINNED without LIVE on a healthy upgraded
// session, which the docs define as "the switch has not been applied yet".
func liveProfileFromPods(cfg *config.DevEnvironment, pods []kube.PodSummary) string {
	unlabelledMatchesSelection := false
	for _, pod := range pods {
		if pod.Deleting {
			continue
		}
		if p := strings.TrimSpace(pod.Labels["okdev.io/workload-profile"]); p != "" {
			return p
		}
		if normalizeWorkloadType(pod.Labels["okdev.io/workload-type"]) == normalizeWorkloadType(cfg.Spec.Workload.Type) {
			unlabelledMatchesSelection = true
		}
	}
	if unlabelledMatchesSelection {
		return cfg.SelectedWorkload()
	}
	return ""
}

func newWorkloadUseCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:               "use <name>",
		Short:             "Switch what the current session runs",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: workloadProfileCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			want := strings.TrimSpace(args[0])
			if err := cc.cfg.SelectWorkload(want); err != nil {
				return err
			}
			live := liveWorkloadProfile(cmd.Context(), cc)
			if err := session.SaveWorkloadProfile(cc.sessionName, want); err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if live != "" && live != want {
				fmt.Fprintf(out, "workload: %s -> %s\n", live, want)
				fmt.Fprintf(out, "session %s is running %s — the next `okdev up` will delete it and recreate as %s\n",
					cc.sessionName, live, want)
			} else {
				fmt.Fprintf(out, "workload: %s\n", want)
			}
			fmt.Fprintln(out, "run `okdev up` to apply")
			return nil
		},
	}
}

func workloadProfileCompletionFunc(opts *Options) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(_ *cobra.Command, _ []string, _ string) ([]string, cobra.ShellCompDirective) {
		cfg, _, err := loadConfigAndNamespace(opts)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return cfg.WorkloadProfileNames(), cobra.ShellCompDirectiveNoFileComp
	}
}

func newWorkloadShowCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:               "show [name]",
		Short:             "Show one workload's resolved settings",
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: workloadProfileCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if len(args) == 1 {
				if err := cc.cfg.SelectWorkload(args[0]); err != nil {
					return err
				}
			}
			w := cc.cfg.Spec.Workload
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "name:      %s\n", cc.cfg.SelectedWorkload())
			fmt.Fprintf(out, "type:      %s\n", normalizeWorkloadType(w.Type))
			if m := strings.TrimSpace(w.ManifestPath); m != "" {
				fmt.Fprintf(out, "manifest:  %s\n", m)
				fmt.Fprintf(out, "resolved:  %s\n", workload.ResolveManifestPath(cc.cfgPath, m))
			} else {
				fmt.Fprintln(out, "manifest:  (synthesized from spec.podTemplate)")
			}
			for _, in := range cc.cfg.EffectiveWorkloadInject() {
				path := in.Path
				if strings.TrimSpace(path) == "" {
					path = "(object root)"
				}
				fmt.Fprintf(out, "inject:    %s\n", path)
			}
			if c := strings.TrimSpace(w.Attach.Container); c != "" {
				fmt.Fprintf(out, "attach:    %s\n", c)
			}
			return nil
		},
	}
}
