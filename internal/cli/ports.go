package cli

import (
	"fmt"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func newPortsCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ports",
		Short: "Reconcile managed SSH port forwards for configured ports",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
			}
			sn, err := resolveSessionNameForUpDown(opts, cfg, ns)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			if err := ensureExistingSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			target, err := loadTargetRef(sn)
			if err != nil {
				return err
			}
			if err := validatePinnedTarget(cmd.Context(), k, ns, target); err != nil {
				return err
			}
			effectiveSSHPort := sshPort
			stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), true, true)
			defer stopMaintenance()
			if len(cfg.Spec.Ports) == 0 {
				return fmt.Errorf("no ports configured in config")
			}
			validForwards := make([]string, 0, len(cfg.Spec.Ports))
			for _, p := range cfg.Spec.Ports {
				if p.Local <= 0 || p.Remote <= 0 {
					continue
				}
				validForwards = append(validForwards, fmt.Sprintf("%d:%d", p.Local, p.Remote))
			}
			if len(validForwards) == 0 {
				return fmt.Errorf("no valid ports configured")
			}

			keyPath, err := defaultSSHKeyPath(cfg)
			if err != nil {
				return fmt.Errorf("resolve SSH key path: %w", err)
			}
			if err := ensureSSHKeyOnPod(opts, ns, target.PodName, keyPath); err != nil {
				return fmt.Errorf("setup SSH key in pod: %w", err)
			}
			if err := waitForSSHReady(opts, ns, target.PodName, 20*time.Second); err != nil {
				return fmt.Errorf("wait for ssh service ready: %w", err)
			}
			alias := sshHostAlias(sn)
			changed, err := ensureSSHConfigEntry(alias, sn, ns, cfg.Spec.SSH.User, effectiveSSHPort, keyPath, cfgPath, cfg.Spec.Ports)
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
				_ = stopManagedSSHForward(alias)
			}
			if err := startManagedSSHForwardWithForwards(alias, cfg.Spec.Ports, cfg.Spec.SSH); err != nil {
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
}
