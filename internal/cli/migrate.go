package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newMigrateCmd(opts *Options) *cobra.Command {
	var dryRun bool
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a .okdev.yaml config to the latest schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
			}

			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("read config %q: %w", cfgPath, err)
			}

			var doc yaml.Node
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parse config %q: %w", cfgPath, err)
			}

			result, err := config.RunMigrations(&doc, config.DefaultMigrations)
			if err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			if len(result.Applied) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "Config is already up to date.")
				return nil
			}

			out, err := yaml.Marshal(&doc)
			if err != nil {
				return fmt.Errorf("marshal migrated config: %w", err)
			}

			w := cmd.OutOrStdout()

			for _, name := range result.Applied {
				fmt.Fprintf(w, "✓ %s\n", name)
			}
			for _, warning := range result.Warnings {
				fmt.Fprintf(w, "  ⚠ %s\n", warning)
			}

			if dryRun {
				fmt.Fprintln(w, "\n--- dry-run output ---")
				_, _ = io.Copy(w, bytes.NewReader(out))
				return nil
			}

			if !noBackup {
				bakPath := cfgPath + ".bak"
				if err := os.WriteFile(bakPath, raw, 0o644); err != nil {
					return fmt.Errorf("write backup %q: %w", bakPath, err)
				}
			}

			if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
				return fmt.Errorf("write migrated config %q: %w", cfgPath, err)
			}

			fmt.Fprintf(w, "\nWrote migrated config to %s", cfgPath)
			if !noBackup {
				fmt.Fprintf(w, " (backup: %s.bak)", cfgPath)
			}
			fmt.Fprintln(w)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print migrated config to stdout without writing")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "Skip creating a .bak backup file")
	return cmd
}
