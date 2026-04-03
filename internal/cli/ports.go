package cli

import (
	"fmt"
	"log/slog"
	"strings"

	"github.com/spf13/cobra"
)

func newPortsCmd(opts *Options) *cobra.Command {
	var dryRun bool
	cmd := &cobra.Command{
		Use:   "ports",
		Short: "Reconcile managed SSH port forwards for configured ports",
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			effectiveSSHPort := sshPort
			if len(cc.cfg.Spec.Ports) == 0 {
				return fmt.Errorf("no ports configured in config")
			}
			validForwards := make([]string, 0, len(cc.cfg.Spec.Ports))
			for _, p := range cc.cfg.Spec.Ports {
				if summary, ok := portMappingSummary(p); ok {
					validForwards = append(validForwards, summary)
				}
			}
			if len(validForwards) == 0 {
				return fmt.Errorf("no valid ports configured")
			}

			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", cc.sessionName, cc.namespace)
				fmt.Fprintf(cmd.OutOrStdout(), "- would reconcile managed forwards for %s\n", sshHostAlias(cc.sessionName))
				for _, summary := range validForwards {
					fmt.Fprintf(cmd.OutOrStdout(), "- %s\n", summary)
				}
				return nil
			}

			stopMaintenance := startSessionMaintenance(opts, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			keyPath, err := defaultSSHKeyPath(cc.cfg)
			if err != nil {
				return fmt.Errorf("resolve SSH key path: %w", err)
			}
			if err := ensureSSHKeyOnPod(opts, cc.namespace, target.PodName, target.Container, keyPath); err != nil {
				return fmt.Errorf("setup SSH key in pod: %w", err)
			}
			if err := waitForSSHReady(opts, cc.namespace, target.PodName, target.Container, sshServiceWaitTimeout); err != nil {
				return fmt.Errorf("wait for ssh service ready: %w", err)
			}
			alias := sshHostAlias(cc.sessionName)
			changed, err := ensureSSHConfigEntry(alias, cc.sessionName, cc.namespace, cc.cfg.Spec.SSH.User, effectiveSSHPort, keyPath, cc.cfgPath, cc.cfg.Spec.Ports)
			if err != nil {
				return fmt.Errorf("update ~/.ssh/config for managed forwards: %w", err)
			}
			running, runErr := isManagedSSHForwardRunning(alias)
			if runErr != nil {
				return fmt.Errorf("check managed SSH forward state: %w", runErr)
			}
			if running && !changed {
				fmt.Fprintf(cmd.OutOrStdout(), "Managed forwards already active for %s: %s\n", alias, strings.Join(validForwards, ", "))
				return nil
			}
			if running {
				if err := stopManagedSSHForward(alias); err != nil {
					slog.Debug("failed to stop managed SSH forward", "alias", alias, "error", err)
				}
			}
			if err := startManagedSSHForwardWithForwards(alias, cc.cfg.Spec.Ports, cc.cfg.Spec.SSH); err != nil {
				return fmt.Errorf("start managed SSH forwards: %w", err)
			}
			if changed {
				fmt.Fprintf(cmd.OutOrStdout(), "Managed forwards updated for %s: %s\n", alias, strings.Join(validForwards, ", "))
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Managed forwards started for %s: %s\n", alias, strings.Join(validForwards, ", "))
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview port-forward actions without changing SSH config or starting forwards")
	return cmd
}
