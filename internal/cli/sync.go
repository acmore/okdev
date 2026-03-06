package cli

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
	var background bool
	var dryRun bool
	var force bool

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
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			renewLock := watch
			if engine == "" {
				engine = cfg.Spec.Sync.Engine
			}
			if engine == "" {
				engine = "native"
			}
			if engine == "syncthing" {
				renewLock = true
			}
			pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.Spec.Workspace.MountPath)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: sync session=%s namespace=%s engine=%s mode=%s\n", sn, ns, engine, mode)
				fmt.Fprintf(cmd.OutOrStdout(), "- paths: %v\n", pairs)
				if watch {
					fmt.Fprintf(cmd.OutOrStdout(), "- watch interval: %s\n", interval)
				}
				if force {
					fmt.Fprintln(cmd.OutOrStdout(), "- force transfer enabled (fingerprint cache bypassed)")
				}
				if background {
					fmt.Fprintln(cmd.OutOrStdout(), "- detached background mode enabled")
				}
				return nil
			}
			stopRenew, err := acquireSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			defer stopRenew()
			stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), renewLock, renewLock)
			defer stopMaintenance()

			pod := podName(sn)

			switch engine {
			case "native":
				if background {
					return fmt.Errorf("--background is only supported with --engine syncthing")
				}
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
						stats, err := syncengine.RunOnceWithOptions(watchCtx, mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude, syncengine.RunOptions{Force: force})
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
						fmt.Fprintf(cmd.OutOrStdout(), "Sync tick completed at %s (paths=%d skipped=%d upload=%dB download=%dB)\n", time.Now().Format(time.RFC3339), stats.Paths, stats.SkippedPaths, stats.UploadBytes, stats.DownloadBytes)
						select {
						case <-sigCh:
							fmt.Fprintln(cmd.OutOrStdout(), "Stopping watch sync")
							return nil
						case <-time.After(interval):
						}
					}
				}
				stats, err := syncengine.RunOnceWithOptions(context.Background(), mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude, syncengine.RunOptions{Force: force})
				if err != nil {
					return fmt.Errorf("run sync once: %w", err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Sync complete (%s/%s) for session %s (paths=%d skipped=%d upload=%dB download=%dB)\n", engine, mode, sn, stats.Paths, stats.SkippedPaths, stats.UploadBytes, stats.DownloadBytes)
				return nil
			case "syncthing":
				if background && os.Getenv("OKDEV_SYNCTHING_BACKGROUND_CHILD") != "1" {
					logPath, err := startDetachedSyncthingSync(opts, mode, sn)
					if err != nil {
						return err
					}
					fmt.Fprintf(cmd.OutOrStdout(), "Started syncthing sync in background for session %s\n", sn)
					fmt.Fprintf(cmd.OutOrStdout(), "Logs: %s\n", logPath)
					return nil
				}
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
	cmd.Flags().BoolVar(&background, "background", false, "Run syncthing sync as a detached background process")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview sync actions without transferring files")
	cmd.Flags().BoolVar(&force, "force", false, "Bypass sync fingerprint cache and force file transfer")
	return cmd
}

func startDetachedSyncthingSync(opts *Options, mode, sessionName string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	logDir := filepath.Join(home, ".okdev", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}
	logPath := filepath.Join(logDir, "syncthing-"+sessionName+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
	}

	args := make([]string, 0, 16)
	if opts.ConfigPath != "" {
		args = append(args, "--config", opts.ConfigPath)
	}
	if opts.Session != "" {
		args = append(args, "--session", opts.Session)
	}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
	}
	if opts.Context != "" {
		args = append(args, "--context", opts.Context)
	}
	if opts.Verbose {
		args = append(args, "--verbose")
	}
	args = append(args, "sync", "--engine", "syncthing", "--mode", mode)

	cmd := exec.Command(exe, args...)
	cmd.Env = append(os.Environ(), "OKDEV_SYNCTHING_BACKGROUND_CHILD=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", err
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	return logPath, nil
}
