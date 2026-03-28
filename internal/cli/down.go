package cli

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newDownCmd(opts *Options) *cobra.Command {
	var deletePVC bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete a dev session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
			ui.section("Validate")
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			ui.stepDone("config", cc.cfgPath)
			ui.stepDone("session", cc.sessionName)
			ui.stepDone("namespace", cc.namespace)
			runtime, err := sessionRuntimeForExisting(cc.cfg, cc.cfgPath, cc.sessionName)
			if err != nil {
				return err
			}
			if err := ensureSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, false); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")
			ctx, cancel := defaultContext()
			defer cancel()
			if dryRun {
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", cc.sessionName, cc.namespace)
				fmt.Fprintf(cmd.OutOrStdout(), "- would delete %s/%s\n", runtime.Kind(), runtime.WorkloadName())
				if deletePVC {
					fmt.Fprintln(cmd.OutOrStdout(), "- note: --delete-pvc is ignored (okdev no longer manages PVC lifecycle)")
				}
				return nil
			}

			if len(cc.cfg.Spec.Agents) > 0 {
				if target, err := resolveTargetRef(ctx, opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube); err != nil {
					ui.warnf("failed to resolve target before agent auth cleanup: %v", err)
				} else {
					results := cleanupConfiguredAgentAuth(ctx, cc.kube, cc.namespace, target.PodName, target.Container, cc.cfg.Spec.Agents, ui.warnf)
					if len(results) > 0 {
						ui.stepDone("agent auth", strings.Join(results, ", "))
					}
				}
			}

			ui.section("Delete")
			ui.stepRun(runtime.Kind(), runtime.WorkloadName())
			if err := runtime.Delete(ctx, cc.kube, cc.namespace, true); err != nil {
				return fmt.Errorf("delete session %s: %w", runtime.Kind(), err)
			}
			ui.stepDone(runtime.Kind(), "deleted")
			if deletePVC {
				ui.warnf("--delete-pvc ignored: okdev no longer manages PVC lifecycle; delete PVCs manually if needed")
			}
			ui.stepDone("pvc", "not managed")
			alias := sshHostAlias(cc.sessionName)
			ui.section("Cleanup")
			if err := session.RequestShutdown(cc.sessionName); err != nil {
				ui.warnf("failed to request shutdown for local clients: %v", err)
			} else {
				ui.stepDone("local clients", "shutdown requested")
			}
			if err := stopDetachedSyncthingSync(cc.sessionName); err != nil {
				ui.warnf("failed to stop background sync: %v", err)
			} else {
				ui.stepDone("sync", "stopped")
			}
			if err := stopLocalSyncthingForSession(cc.sessionName); err != nil {
				ui.warnf("failed to stop local syncthing: %v", err)
			} else {
				ui.stepDone("syncthing", "stopped")
			}
			if err := stopManagedSSHForward(alias); err != nil {
				slog.Debug("failed to stop managed SSH forward", "alias", alias, "error", err)
			}
			ui.stepDone("ssh forward", "stopped")
			if err := removeSSHConfigEntry(alias); err != nil {
				ui.warnf("failed to remove SSH config entry for %s: %v", alias, err)
			} else {
				ui.stepDone("ssh config", "cleaned")
			}
			if active, err := session.LoadActiveSession(); err == nil && active == cc.sessionName {
				if clearErr := session.ClearActiveSession(); clearErr != nil {
					ui.warnf("failed to clear active session: %v", clearErr)
				} else {
					ui.stepDone("active session", "cleared")
				}
			}
			if err := session.ClearTarget(cc.sessionName); err != nil {
				ui.warnf("failed to clear target state: %v", err)
			} else {
				ui.stepDone("target", "cleared")
			}
			ui.printWarnings()
			ui.section("Ready")
			fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", cc.sessionName)
			fmt.Fprintf(cmd.OutOrStdout(), "namespace: %s\n", cc.namespace)
			fmt.Fprintln(cmd.OutOrStdout(), "status:    stopped")
			fmt.Fprintln(cmd.OutOrStdout(), "workspace: pod deleted; volumes/PVCs unchanged")
			return nil
		},
	}
	cmd.Flags().BoolVar(&deletePVC, "delete-pvc", false, "Delete workspace PVC for this session")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without deleting resources")
	_ = cmd.Flags().MarkDeprecated("delete-pvc", "PVC lifecycle is no longer managed; delete PVCs manually if needed")
	return cmd
}

func removeSSHConfigEntry(hostAlias string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")
	begin := "# BEGIN OKDEV " + hostAlias
	end := "# END OKDEV " + hostAlias
	if _, err := os.Stat(configPath); err != nil {
		return nil
	}
	return updateSSHConfigWithLock(configPath, func(existing string) string {
		updated := strings.TrimSpace(stripManagedSSHBlock(existing, begin, end))
		if updated == "" {
			return ""
		}
		return updated + "\n"
	})
}
