package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func newValidateCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Validate okdev config",
		RunE: func(cmd *cobra.Command, args []string) error {
			_, path, err := config.Load(opts.ConfigPath)
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Config valid: %s\n", path)
			return nil
		},
	}
}
