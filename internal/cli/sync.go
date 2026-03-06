package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

func newSyncCmd(opts *Options) *cobra.Command {
	var mode string
	var engine string
	var watch bool
	var interval time.Duration

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync files between local workspace and session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			renewLock := watch
			if engine == "" {
				engine = cfg.Spec.Sync.Engine
			}
			if engine == "syncthing" {
				renewLock = true
			}
			stopRenew, err := acquireSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout(), renewLock)
			if err != nil {
				return err
			}
			defer stopRenew()
			stopHeartbeat := func() {}
			if renewLock {
				stopHeartbeat = startSessionHeartbeat(opts, ns, sn, cmd.OutOrStdout(), time.Minute)
			}
			defer stopHeartbeat()
			if engine == "" {
				engine = cfg.Spec.Sync.Engine
			}
			if engine == "" {
				engine = "native"
			}
			pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.Spec.Workspace.MountPath)
			if err != nil {
				return err
			}

			k := newKubeClient(opts)
			pod := podName(sn)

			switch engine {
			case "native":
				if watch {
					fmt.Fprintf(cmd.OutOrStdout(), "Starting native watch sync (%s) for session %s every %s\n", mode, sn, interval)
					sigCh := make(chan os.Signal, 1)
					signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
					defer signal.Stop(sigCh)
					watchCtx, stopWatch := context.WithCancel(context.Background())
					defer stopWatch()
					backoff := 1 * time.Second
					for {
						select {
						case <-sigCh:
							stopWatch()
							fmt.Fprintln(cmd.OutOrStdout(), "Stopping watch sync")
							return nil
						default:
						}
						stats, err := syncengine.RunOnceWithReport(watchCtx, mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude)
						if err != nil {
							slog.Warn("watch sync tick failed", "namespace", ns, "session", sn, "pod", pod, "mode", mode, "backoff", backoff.String(), "error", err)
							fmt.Fprintf(cmd.ErrOrStderr(), "sync tick failed: %v (retrying in %s)\n", err, backoff)
							select {
							case <-sigCh:
								fmt.Fprintln(cmd.OutOrStdout(), "Stopping watch sync")
								return nil
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
						backoff = 1 * time.Second
						slog.Debug("watch sync tick completed", "namespace", ns, "session", sn, "pod", pod, "mode", mode)
						fmt.Fprintf(cmd.OutOrStdout(), "Sync tick completed at %s (paths=%d upload=%dB download=%dB)\n", time.Now().Format(time.RFC3339), stats.Paths, stats.UploadBytes, stats.DownloadBytes)
						select {
						case <-sigCh:
							fmt.Fprintln(cmd.OutOrStdout(), "Stopping watch sync")
							return nil
						case <-time.After(interval):
						}
					}
				}
				stats, err := syncengine.RunOnceWithReport(context.Background(), mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude)
				if err != nil {
					return fmt.Errorf("run sync once: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Sync complete (%s/%s) for session %s (paths=%d upload=%dB download=%dB)\n", engine, mode, sn, stats.Paths, stats.UploadBytes, stats.DownloadBytes)
				return nil
			case "syncthing":
				return runSyncthingSync(cmd, opts, cfg, ns, sn, mode, pairs)
			default:
				return fmt.Errorf("unsupported sync engine %q (supported: native|syncthing)", engine)
			}
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	cmd.Flags().StringVar(&engine, "engine", "", "Sync engine override: native|syncthing")
	cmd.Flags().BoolVar(&watch, "watch", false, "Continuously sync in a loop (native engine)")
	cmd.Flags().DurationVar(&interval, "interval", 2*time.Second, "Watch loop interval (native engine)")
	return cmd
}
