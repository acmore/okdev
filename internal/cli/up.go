package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strconv"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	var attach bool
	var waitTimeout time.Duration
	var dryRun bool

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
			labels := labelsForSession(cfg, sn)
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
				if attach {
					fmt.Fprintln(cmd.OutOrStdout(), "- would attach shell and start background sync/ports")
				}
				return nil
			}
			stopRenew, err := acquireSessionLockWithClient(k, cfg, ns, sn, cmd.OutOrStdout(), attach)
			if err != nil {
				return err
			}
			defer stopRenew()
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
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without applying resources")
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
	pods, err := k.ListPods(ctx, namespace, false, "okdev.io/managed=true,okdev.io/session="+sessionName)
	if err != nil {
		return err
	}
	for _, p := range pods {
		if p.Name != podName {
			continue
		}
		if cfgInfo.ModTime().After(p.CreatedAt) {
			fmt.Fprintf(errOut, "warning: config file is newer than running session pod (%s > %s). Run `okdev down --session %s` then `okdev up --session %s` to apply all changes.\n", cfgInfo.ModTime().UTC().Format(time.RFC3339), p.CreatedAt.UTC().Format(time.RFC3339), sessionName, sessionName)
		}
		break
	}
	return nil
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
