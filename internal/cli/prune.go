package cli

import (
	"fmt"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func newPruneCmd(opts *Options) *cobra.Command {
	var allNamespaces bool
	var ttlHours int
	var includePVC bool
	var dryRun bool

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
			candidates := 0
			for _, p := range pods {
				if p.CreatedAt.IsZero() {
					continue
				}
				ttlExpired := now.Sub(p.CreatedAt) >= time.Duration(ttlHours)*time.Hour
				idleExpired := false
				idleReason := ""
				if idleRaw := p.Annotations["okdev.io/idle-timeout-minutes"]; idleRaw != "" {
					idleMinutes, err := strconv.Atoi(idleRaw)
					if err == nil && idleMinutes > 0 {
						lastActive := p.CreatedAt
						if ts := p.Annotations["okdev.io/last-attach"]; ts != "" {
							if parsed, parseErr := time.Parse(time.RFC3339, ts); parseErr == nil {
								lastActive = parsed
							}
						}
						if now.Sub(lastActive) >= time.Duration(idleMinutes)*time.Minute {
							idleExpired = true
							idleReason = fmt.Sprintf("idle>%dm", idleMinutes)
						}
					}
				}
				if !ttlExpired && !idleExpired {
					continue
				}
				candidates++
				sessionName := p.Labels["okdev.io/session"]
				if sessionName == "" {
					sessionName = p.Name
				}
				reason := fmt.Sprintf("ttl>%dh", ttlHours)
				if idleReason != "" && !ttlExpired {
					reason = idleReason
				} else if idleReason != "" && ttlExpired {
					reason = reason + "," + idleReason
				}
				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: would prune session %s in namespace %s (%s)\n", sessionName, p.Namespace, reason)
					if includePVC {
						fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: would delete pvc/okdev-%s-workspace in namespace %s\n", sessionName, p.Namespace)
					}
					continue
				}
				if err := k.Delete(ctx, p.Namespace, "pod", p.Name, true); err != nil {
					return err
				}
				if includePVC {
					_ = k.Delete(ctx, p.Namespace, "pvc", "okdev-"+sessionName+"-workspace", true)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Pruned session %s in namespace %s (%s)\n", sessionName, p.Namespace, reason)
				deleted++
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: prune candidate count=%d\n", candidates)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Prune complete, deleted %d session(s)\n", deleted)
			return nil
		},
	}

	cmd.Flags().BoolVar(&allNamespaces, "all-namespaces", false, "Prune sessions across all namespaces")
	cmd.Flags().IntVar(&ttlHours, "ttl-hours", 0, "TTL in hours override")
	cmd.Flags().BoolVar(&includePVC, "include-pvc", false, "Delete default workspace PVCs for pruned sessions")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview prune actions without deleting resources")
	return cmd
}
