package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/upgrade"
	"github.com/acmore/okdev/internal/version"
	"github.com/spf13/cobra"
)

func newUpgradeCmd() *cobra.Command {
	var allowUnattested bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Upgrade okdev to the latest version",
		Long: `Upgrade okdev to the latest release.

The downloaded archive must match the release checksums and carry a GitHub
build provenance attestation (Sigstore) signed by okdev's release workflow
for that exact tag; otherwise nothing is installed.`,
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
			upgrader.AllowUnattested = allowUnattested
			upgrader.Warn = cmd.ErrOrStderr()
			if err := upgrader.Upgrade(); err != nil {
				return fmt.Errorf("upgrade failed: %w", err)
			}
			fmt.Fprintln(cmd.OutOrStdout(), "Upgraded to", latest)
			return nil
		},
	}
	cmd.Flags().BoolVar(&allowUnattested, "allow-unattested", false, "Install a release that has no build provenance attestation (checksum only); never bypasses a failed verification")
	return cmd
}
