package cli

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func newPruneCmd(opts *Options) *cobra.Command {
	var allNamespaces bool
	var allUsers bool
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
			label := "okdev.io/managed=true"
			if !allUsers {
				label = label + "," + ownerLabelSelector(opts)
			}
			pods, err := k.ListPods(ctx, ns, allNamespaces, label)
			if err != nil {
				return err
			}
			views := buildSessionViews(pods)

			now := time.Now()
			deleted := 0
			candidates := 0
			for _, view := range views {
				targetPod, ok := sessionTargetPod(view)
				if !ok || targetPod.CreatedAt.IsZero() {
					continue
				}
				ttlExpired := now.Sub(view.CreatedAt) >= time.Duration(ttlHours)*time.Hour
				idleExpired := false
				idleReason := ""
				if idleRaw := targetPod.Annotations["okdev.io/idle-timeout-minutes"]; idleRaw != "" {
					idleMinutes, err := strconv.Atoi(idleRaw)
					if err == nil && idleMinutes > 0 {
						lastActive := targetPod.CreatedAt
						if ts := targetPod.Annotations["okdev.io/last-attach"]; ts != "" {
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
				sessionName := view.Session
				reason := fmt.Sprintf("ttl>%dh", ttlHours)
				if idleReason != "" && !ttlExpired {
					reason = idleReason
				} else if idleReason != "" && ttlExpired {
					reason = reason + "," + idleReason
				}
				if dryRun {
					fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: would prune session %s in namespace %s (%s)\n", sessionName, view.Namespace, reason)
					if includePVC {
						fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: would delete pvc/okdev-%s-workspace in namespace %s\n", sessionName, view.Namespace)
					}
					continue
				}
				if err := deleteSessionWorkload(ctx, k, view, targetPod); err != nil {
					return err
				}
				if includePVC {
					_ = k.Delete(ctx, view.Namespace, "pvc", "okdev-"+sessionName+"-workspace", true)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Pruned session %s in namespace %s (%s)\n", sessionName, view.Namespace, reason)
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
	cmd.Flags().BoolVar(&allUsers, "all-users", false, "Prune sessions for all owners")
	cmd.Flags().IntVar(&ttlHours, "ttl-hours", 0, "TTL in hours override")
	cmd.Flags().BoolVar(&includePVC, "include-pvc", false, "Delete default workspace PVCs for pruned sessions")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview prune actions without deleting resources")
	return cmd
}

func sessionTargetPod(view sessionView) (kube.PodSummary, bool) {
	for _, pod := range view.Pods {
		if pod.Name == view.TargetPod {
			return pod, true
		}
	}
	if len(view.Pods) == 0 {
		return kube.PodSummary{}, false
	}
	return view.Pods[0], true
}

func deleteSessionWorkload(ctx context.Context, k *kube.Client, view sessionView, targetPod kube.PodSummary) error {
	apiVersion := strings.TrimSpace(targetPod.Annotations["okdev.io/workload-api-version"])
	if apiVersion == "" {
		apiVersion = strings.TrimSpace(targetPod.Labels["okdev.io/workload-api-version"])
	}
	resourceKind := strings.TrimSpace(targetPod.Annotations["okdev.io/workload-resource-kind"])
	if resourceKind == "" {
		resourceKind = strings.TrimSpace(targetPod.Labels["okdev.io/workload-resource-kind"])
	}
	workloadName := strings.TrimSpace(targetPod.Annotations["okdev.io/workload-name"])
	if workloadName == "" {
		workloadName = strings.TrimSpace(targetPod.Labels["okdev.io/workload-name"])
	}
	if apiVersion != "" && resourceKind != "" && workloadName != "" {
		return k.DeleteByRef(ctx, view.Namespace, apiVersion, resourceKind, workloadName, true)
	}
	if strings.TrimSpace(view.WorkloadType) != "" && view.WorkloadType != string(workload.TypePod) {
		slog.Warn("missing workload metadata on controller-backed session pod; falling back to pod delete", "session", view.Session, "namespace", view.Namespace, "workloadType", view.WorkloadType, "pod", targetPod.Name)
	}
	return k.Delete(ctx, view.Namespace, "pod", targetPod.Name, true)
}
