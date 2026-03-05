package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter .okdev.yaml config",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := config.DefaultFile
			if opts.ConfigPath != "" {
				target = opts.ConfigPath
			}

			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve output path %q: %w", target, err)
			}

			if _, err := os.Stat(abs); err == nil && !force {
				return fmt.Errorf("config already exists at %q (use --force to overwrite)", abs)
			}

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
			if err := os.WriteFile(abs, []byte(config.DefaultTemplate), 0o644); err != nil {
				return fmt.Errorf("write config %q: %w", abs, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Created %s\n", abs)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	return cmd
}
