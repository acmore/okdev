package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/upgrade"
	"github.com/acmore/okdev/internal/version"
	"github.com/spf13/cobra"
)

func newUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade okdev to the latest version",
		RunE: func(cmd *cobra.Command, args []string) error {
			checker := upgrade.NewChecker()
			latest, err := checker.LatestVersion()
			if err != nil {
				return fmt.Errorf("check latest version: %w", err)
			}
			current := version.Version
			if !upgrade.ShouldUpgrade(current, latest) {
				fmt.Fprintln(cmd.OutOrStdout(), "Already up to date:", current)
				return nil
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Upgrading okdev %s → %s\n", current, latest)
			upgrader, err := upgrade.NewUpgrader(latest)
			if err != nil {
				return err
			}
			if err := upgrader.Upgrade(); err != nil {
				return fmt.Errorf("upgrade failed: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Upgraded to", latest)
			return nil
		},
	}
}
