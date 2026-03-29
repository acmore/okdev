package cli

import (
	"context"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func initRootCompletion(cmd *cobra.Command) {
	cmd.InitDefaultCompletionCmd()
}

func sessionCompletionFunc(opts *Options) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		if len(args) > 0 {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		namespace := strings.TrimSpace(opts.Namespace)
		if namespace == "" {
			if path, err := config.ResolvePath(opts.ConfigPath); err == nil {
				if cfg, _, err := config.Load(path); err == nil {
					applyConfigKubeContext(opts, cfg)
					if ns := strings.TrimSpace(cfg.Spec.Namespace); ns != "" {
						namespace = ns
					}
				}
			}
		}
		if namespace == "" {
			namespace = "default"
		}

		ctx, cancel := context.WithTimeout(context.Background(), sessionExistsTimeout)
		defer cancel()
		label := "okdev.io/managed=true," + ownerLabelSelector(opts)
		pods, err := newKubeClient(opts).ListPods(ctx, namespace, false, label)
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		seen := map[string]struct{}{}
		names := make([]string, 0, len(pods))
		for _, pod := range pods {
			name := sessionNameFromPodSummary(pod)
			if strings.TrimSpace(name) == "" {
				continue
			}
			if toComplete != "" && !strings.HasPrefix(name, toComplete) {
				continue
			}
			if _, ok := seen[name]; ok {
				continue
			}
			seen[name] = struct{}{}
			names = append(names, name)
		}
		sort.Strings(names)
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}
