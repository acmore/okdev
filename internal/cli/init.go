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
	var templateRef string
	var yes bool
	var nameOverride string
	var nsOverride string

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

			vars := config.NewTemplateVars()
			if nameOverride != "" {
				vars.Name = nameOverride
			}
			if nsOverride != "" {
				vars.Namespace = nsOverride
			}
			if vars.Name == "" {
				wd, _ := os.Getwd()
				if wd != "" {
					vars.Name = filepath.Base(wd)
				} else {
					vars.Name = "my-project"
				}
			}

			// TODO: interactive prompts will be added in Task 8/9
			// For now, non-interactive only (--yes is implicit until prompts are wired)
			_ = yes

			rendered, err := config.RenderTemplate(templateRef, vars)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
			if err := os.WriteFile(abs, []byte(rendered), 0o644); err != nil {
				return fmt.Errorf("write config %q: %w", abs, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", abs)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().StringVar(&templateRef, "template", "basic", "Template: built-in name, file path, or URL")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept all defaults")
	cmd.Flags().StringVar(&nameOverride, "name", "", "Environment name")
	cmd.Flags().StringVar(&nsOverride, "namespace", "", "Namespace")
	return cmd
}
