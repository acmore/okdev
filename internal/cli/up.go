package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	var waitTimeout time.Duration
	var dryRun bool
	var tmux bool

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a dev session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			sn, err := resolveSessionNameForUpDown(opts, cfg, ns)
			if err != nil {
				return err
			}
			labels := labelsForSession(opts, cfg, sn)
			annotations := annotationsForSession(cfg)
			pvc := pvcName(cfg, sn)
			pod := podName(sn)
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", sn, ns)
				if cfg.Spec.Workspace.PVC.ClaimName == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "- would apply pvc/%s\n", pvc)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "- using existing pvc/%s\n", pvc)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "- would apply pod/%s\n", pod)
				fmt.Fprintf(cmd.OutOrStdout(), "- would wait for pod readiness (timeout=%s)\n", waitTimeout)
				fmt.Fprintln(cmd.OutOrStdout(), "- would setup SSH config + managed SSH/port-forwards")
				fmt.Fprintln(cmd.OutOrStdout(), "- would start background sync (when sync.engine=syncthing)")
				return nil
			}
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			if warnErr := warnIfConfigNewerThanSession(opts, k, ns, sn, pod, cmd.ErrOrStderr()); warnErr != nil {
				slog.Debug("skip config drift warning", "error", warnErr)
			}
			ctx, cancel := defaultContext()
			defer cancel()

			if cfg.Spec.Workspace.PVC.ClaimName == "" {
				pvcManifest, err := kube.BuildPVCManifest(ns, pvc, cfg.Spec.Workspace.PVC.Size, cfg.Spec.Workspace.PVC.StorageClassName, labels, annotations)
				if err != nil {
					return err
				}
				if err := k.Apply(ctx, ns, pvcManifest); err != nil {
					return err
				}
			}

			enableTmux := tmux || cfg.Spec.SSH.PersistentSessionEnabled()
			preparedSpec, err := kube.PreparePodSpec(
				cfg.Spec.PodTemplate.Spec,
				pvc,
				cfg.Spec.Workspace.MountPath,
				cfg.Spec.Sidecar.Image,
				enableTmux,
			)
			if err != nil {
				return err
			}

			podManifest, err := kube.BuildPodManifest(ns, pod, labels, annotations, preparedSpec)
			if err != nil {
				return err
			}
			if err := k.Apply(ctx, ns, podManifest); err != nil {
				return err
			}
			progressPrinter := func(p kube.PodReadinessProgress) {
				fmt.Fprintf(cmd.OutOrStdout(), "Waiting for pod/%s: phase=%s ready=%d/%d reason=%s\n", pod, p.Phase, p.ReadyContainers, p.TotalContainers, p.Reason)
			}
			if err := k.WaitReadyWithProgress(ctx, ns, pod, waitTimeout, progressPrinter); err != nil {
				hints := fmt.Sprintf("next steps:\n- run `okdev status --session %s`\n- run `kubectl -n %s describe pod %s`", sn, ns, pod)
				diag, derr := k.DescribePod(ctx, ns, pod)
				if derr == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "pod diagnostics:\n%s\n\n%s\n", diag, hints)
					return fmt.Errorf("wait for pod/%s readiness failed: %w", pod, err)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), hints)
				return fmt.Errorf("wait for pod/%s readiness failed: %w", pod, err)
			}

			if err := session.SaveActiveSession(sn); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Session ready: %s (namespace: %s)\n", sn, ns)
			if cfg.Spec.SSH.RemotePort > 0 {
				keyPath, keyErr := defaultSSHKeyPath(cfg)
				if keyErr != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to resolve SSH key path: %v\n", keyErr)
				} else {
					if err := ensureSSHKeyOnPod(opts, cfg, ns, pod, keyPath); err != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to setup SSH key in pod: %v\n", err)
					}
					alias := sshHostAlias(sn)
					cfgPath, _ := config.ResolvePath(opts.ConfigPath)
					if cfgErr := ensureSSHConfigEntry(alias, sn, ns, cfg.Spec.SSH.User, cfg.Spec.SSH.RemotePort, keyPath, cfgPath, cfg.Spec.Ports); cfgErr != nil {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
					} else {
						// Force-refresh managed master so forward rules are always current after `okdev up`.
						_ = stopManagedSSHForward(alias)
						if err := startManagedSSHForward(alias); err != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to start managed SSH/port-forwards: %v\n", err)
						} else {
							if len(cfg.Spec.Ports) > 0 {
								fmt.Fprintf(cmd.OutOrStdout(), "SSH ready: ssh %s (managed forwards active)\n", alias)
							} else {
								fmt.Fprintf(cmd.OutOrStdout(), "SSH ready: ssh %s\n", alias)
							}
						}
					}
				}
			}

			if cfg.Spec.Sync.Engine == "" || cfg.Spec.Sync.Engine == "syncthing" {
				logPath, started, err := startDetachedSyncthingSync(opts, "bi", sn)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to start syncthing background sync: %v\n", err)
				} else {
					if started {
						fmt.Fprintf(cmd.OutOrStdout(), "Background syncthing sync active (mode=bi). Logs: %s\n", logPath)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "Background syncthing sync already running (mode=bi). Logs: %s\n", logPath)
					}
				}
			}

			return nil
		},
	}

	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Wait timeout for pod readiness")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without applying resources")
	cmd.Flags().BoolVar(&tmux, "tmux", false, "Enable tmux persistent shell sessions in the sidecar")
	return cmd
}

func warnIfConfigNewerThanSession(opts *Options, k *kube.Client, namespace, sessionName, podName string, errOut io.Writer) error {
	cfgPath, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return err
	}
	cfgInfo, err := os.Stat(cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	podSummary, err := k.GetPodSummary(ctx, namespace, podName)
	if err != nil {
		return err
	}
	if cfgInfo.ModTime().After(podSummary.CreatedAt) {
		fmt.Fprintf(errOut, "warning: config file is newer than running session pod (%s > %s). Run `okdev down --session %s` then `okdev up --session %s` to apply all changes.\n", cfgInfo.ModTime().UTC().Format(time.RFC3339), podSummary.CreatedAt.UTC().Format(time.RFC3339), sessionName, sessionName)
	}
	return nil
}
