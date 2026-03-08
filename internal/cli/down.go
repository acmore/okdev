package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			cfgPath, pathErr := config.ResolvePath(opts.ConfigPath)
			if pathErr == nil {
				ui.stepDone("config", cfgPath)
			}
			sn, err := resolveSessionNameForUpDown(opts, cfg, ns)
			if err != nil {
				return err
			}
			ui.stepDone("session", sn)
			ui.stepDone("namespace", ns)
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, false); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")
			ctx, cancel := defaultContext()
			defer cancel()
			if dryRun {
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", sn, ns)
				fmt.Fprintf(cmd.OutOrStdout(), "- would delete pod/%s\n", podName(sn))
				if deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "- would delete pvc/%s\n", pvcName(cfg, sn))
				} else if !deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "- would retain pvc/%s\n", pvcName(cfg, sn))
				}
				return nil
			}

			ui.section("Delete")
			ui.stepRun("pod", podName(sn))
			if err := k.Delete(ctx, ns, "pod", podName(sn), true); err != nil && !apierrors.IsNotFound(err) {
				return fmt.Errorf("delete session pod: %w", err)
			}
			ui.stepDone("pod", "deleted")
			if deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
				ui.stepRun("pvc", pvcName(cfg, sn))
				if err := k.Delete(ctx, ns, "pvc", pvcName(cfg, sn), true); err != nil && !apierrors.IsNotFound(err) {
					return fmt.Errorf("delete workspace pvc: %w", err)
				}
				ui.stepDone("pvc", "deleted")
			} else if cfg.Spec.Workspace.PVC.ClaimName == "" {
				ui.stepDone("pvc", "retained")
			} else {
				ui.stepDone("pvc", "external claim")
			}
			alias := sshHostAlias(sn)
			ui.section("Cleanup")
			_ = stopManagedSSHForward(alias)
			ui.stepDone("ssh forward", "stopped")
			if err := removeSSHConfigEntry(alias); err != nil {
				ui.warnf("failed to remove SSH config entry for %s: %v", alias, err)
			} else {
				ui.stepDone("ssh config", "cleaned")
			}
			if active, err := session.LoadActiveSession(); err == nil && active == sn {
				_ = session.ClearActiveSession()
				ui.stepDone("active session", "cleared")
			}
			ui.printWarnings()
			ui.section("Ready")
			fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", sn)
			fmt.Fprintf(cmd.OutOrStdout(), "namespace: %s\n", ns)
			fmt.Fprintln(cmd.OutOrStdout(), "status:    stopped")
			if !deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "workspace: retained (%s)\n", pvcName(cfg, sn))
				fmt.Fprintln(cmd.OutOrStdout(), "note:      use --delete-pvc to remove workspace storage")
			} else if deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
				fmt.Fprintln(cmd.OutOrStdout(), "workspace: deleted")
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "workspace: external claim (%s)\n", cfg.Spec.Workspace.PVC.ClaimName)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deletePVC, "delete-pvc", false, "Delete workspace PVC for this session")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without deleting resources")
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
