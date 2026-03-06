package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	var attach bool
	var waitTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a dev session",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			stopRenew, err := acquireSessionLockWithClient(k, cfg, ns, sn, cmd.OutOrStdout(), attach)
			if err != nil {
				return err
			}
			defer stopRenew()
			labels := labelsForSession(cfg, sn)
			annotations := annotationsForSession(cfg)
			pvc := pvcName(cfg, sn)
			pod := podName(sn)

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

			preparedSpec, err := kube.PreparePodSpec(
				cfg.Spec.PodTemplate.Spec,
				pvc,
				cfg.Spec.Workspace.MountPath,
				cfg.Spec.Sync.Engine == "syncthing",
				cfg.Spec.Sync.Syncthing.Image,
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
					return fmt.Errorf("%w\n\npod diagnostics:\n%s\n\n%s", err, diag, hints)
				}
				return fmt.Errorf("%w\n\n%s", err, hints)
			}

			if err := session.SaveActiveSession(sn); err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Session ready: %s (namespace: %s)\n", sn, ns)
			if attach {
				stopMaintenance := startSessionMaintenanceWithClient(k, cfg, ns, sn, cmd.OutOrStdout(), true, true)
				defer stopMaintenance()

				stopBackgrounds := make([]func(), 0, 2)
				defer func() {
					for _, stop := range stopBackgrounds {
						stop()
					}
				}()

				if len(cfg.Spec.Ports) > 0 {
					forwards := make([]string, 0, len(cfg.Spec.Ports))
					for _, p := range cfg.Spec.Ports {
						if p.Local <= 0 || p.Remote <= 0 {
							continue
						}
						forwards = append(forwards, strconv.Itoa(p.Local)+":"+strconv.Itoa(p.Remote))
					}
					if len(forwards) > 0 {
						cancelPF, err := startManagedPortForwardWithClient(k, ns, pod, forwards)
						if err != nil {
							return fmt.Errorf("start background port-forward: %w", err)
						}
						stopBackgrounds = append(stopBackgrounds, cancelPF)
						fmt.Fprintf(cmd.OutOrStdout(), "Background port-forward active: %v\n", forwards)
					}
				}

				engine := cfg.Spec.Sync.Engine
				if engine == "" {
					engine = "native"
				}
				switch engine {
				case "native":
					pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.Spec.Workspace.MountPath)
					if err != nil {
						return err
					}
					syncCtx, cancelSync := context.WithCancel(context.Background())
					stopBackgrounds = append(stopBackgrounds, cancelSync)
					go runAttachNativeSyncLoop(syncCtx, cmd.OutOrStdout(), cmd.ErrOrStderr(), k, ns, pod, pairs, cfg.Spec.Sync.Exclude, 2*time.Second)
					fmt.Fprintln(cmd.OutOrStdout(), "Background file sync active (native/watch)")
				case "syncthing":
					fmt.Fprintln(cmd.OutOrStdout(), "Sync engine is syncthing; run `okdev sync --engine syncthing` in another terminal to start it.")
				}

				return runConnectWithClient(k, ns, sn, []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}, true)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&attach, "attach", false, "Attach shell after session is ready")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Wait timeout for pod readiness")
	return cmd
}

func runAttachNativeSyncLoop(ctx context.Context, out io.Writer, errOut io.Writer, k *kube.Client, namespace, pod string, pairs []syncengine.Pair, excludes []string, interval time.Duration) {
	backoff := time.Second
	slog.Debug("background sync loop started", "namespace", namespace, "pod", pod, "interval", interval.String())
	defer slog.Debug("background sync loop stopped", "namespace", namespace, "pod", pod)
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		stats, err := syncengine.RunOnceWithReport(ctx, "bi", k, namespace, pod, pairs, excludes)
		if err != nil {
			slog.Warn("background sync tick failed", "namespace", namespace, "pod", pod, "backoff", backoff.String(), "error", err)
			fmt.Fprintf(errOut, "background sync tick failed: %v (retry in %s)\n", err, backoff)
			select {
			case <-ctx.Done():
				return
			case <-time.After(backoff):
			}
			if backoff < 30*time.Second {
				backoff *= 2
				if backoff > 30*time.Second {
					backoff = 30 * time.Second
				}
			}
			continue
		}
		backoff = time.Second
		slog.Debug("background sync tick completed", "namespace", namespace, "pod", pod)
		fmt.Fprintf(out, "Background sync tick completed at %s (paths=%d skipped=%d upload=%dB download=%dB)\n", time.Now().Format(time.RFC3339), stats.Paths, stats.SkippedPaths, stats.UploadBytes, stats.DownloadBytes)
		select {
		case <-ctx.Done():
			return
		case <-time.After(interval):
		}
	}
}
