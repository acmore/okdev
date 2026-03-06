package cli

import (
	"fmt"
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
			if err := ensureSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout()); err != nil {
				return err
			}
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
					backoff := 1 * time.Second
					for {
						select {
						case <-sigCh:
							fmt.Fprintln(cmd.OutOrStdout(), "Stopping watch sync")
							return nil
						default:
						}
						if err := syncengine.RunOnce(mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude); err != nil {
							fmt.Fprintf(cmd.ErrOrStderr(), "sync tick failed: %v (retrying in %s)\n", err, backoff)
							time.Sleep(backoff)
							if backoff < 30*time.Second {
								backoff *= 2
								if backoff > 30*time.Second {
									backoff = 30 * time.Second
								}
							}
							continue
						}
						backoff = 1 * time.Second
						fmt.Fprintf(cmd.OutOrStdout(), "Sync tick completed at %s\n", time.Now().Format(time.RFC3339))
						time.Sleep(interval)
					}
				}
				if err := syncengine.RunOnce(mode, k, ns, pod, pairs, cfg.Spec.Sync.Exclude); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Sync complete (%s/%s) for session %s\n", engine, mode, sn)
				if mode == "bi" {
					fmt.Fprintln(cmd.OutOrStdout(), "Note: bi mode currently performs upload then download in sequence")
				}
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
