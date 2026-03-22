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
	var sidecarImageOverride string
	var syncLocalOverride string
	var syncRemoteOverride string
	var sshUserOverride string

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
			overrides := InitOverrides{
				Name:         nameOverride,
				Namespace:    nsOverride,
				SidecarImage: sidecarImageOverride,
				SyncLocal:    syncLocalOverride,
				SyncRemote:   syncRemoteOverride,
				SSHUser:      sshUserOverride,
			}
			applyOverrides(vars, overrides)

			if err := promptInteractive(vars, overrides, cmd.InOrStdin(), cmd.OutOrStdout(), yes, isTerminalReader(cmd.InOrStdin())); err != nil {
				return err
			}

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
	cmd.Flags().StringVar(&sidecarImageOverride, "sidecar-image", "", "Sidecar image")
	cmd.Flags().StringVar(&syncLocalOverride, "sync-local", "", "Local sync path")
	cmd.Flags().StringVar(&syncRemoteOverride, "sync-remote", "", "Remote sync path")
	cmd.Flags().StringVar(&sshUserOverride, "ssh-user", "", "SSH user")
	return cmd
}
