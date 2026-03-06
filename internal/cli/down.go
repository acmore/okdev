package cli

import (
	"fmt"

	"github.com/spf13/cobra"
)

func newDownCmd(opts *Options) *cobra.Command {
	var deletePVC bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete a dev session pod",
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
			if err := ensureSessionOwnership(opts, k, ns, sn, false); err != nil {
				return err
			}
			ctx, cancel := defaultContext()
			defer cancel()
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", sn, ns)
				fmt.Fprintf(cmd.OutOrStdout(), "- would delete pod/%s\n", podName(sn))
				if deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "- would delete pvc/%s\n", pvcName(cfg, sn))
				} else if !deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
					fmt.Fprintf(cmd.OutOrStdout(), "- would retain pvc/%s\n", pvcName(cfg, sn))
				}
				fmt.Fprintf(cmd.OutOrStdout(), "- would delete lease/%s\n", "okdev-"+sn)
				return nil
			}

			if err := k.Delete(ctx, ns, "pod", podName(sn), true); err != nil {
				return fmt.Errorf("delete session pod: %w", err)
			}
			if deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
				if err := k.Delete(ctx, ns, "pvc", pvcName(cfg, sn), true); err != nil {
					return fmt.Errorf("delete workspace pvc: %w", err)
				}
			}
			if err := k.Delete(ctx, ns, "lease", "okdev-"+sn, true); err != nil {
				return fmt.Errorf("delete session lease: %w", err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Session stopped: %s\n", sn)
			if !deletePVC && cfg.Spec.Workspace.PVC.ClaimName == "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Workspace PVC retained: %s (use --delete-pvc to remove)\n", pvcName(cfg, sn))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&deletePVC, "delete-pvc", false, "Delete workspace PVC for this session")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without deleting resources")
	return cmd
}
