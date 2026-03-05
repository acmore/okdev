package cli

import (
	"strings"

	"github.com/spf13/cobra"
)

func newConnectCmd(opts *Options) *cobra.Command {
	var shell string
	var cmdStr string
	var noTTY bool

	cmd := &cobra.Command{
		Use:   "connect",
		Short: "Open shell or run command in session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}

			execCmd := []string{shell}
			if cmdStr != "" {
				execCmd = []string{"sh", "-lc", cmdStr}
			}
			if len(args) > 0 {
				execCmd = args
			}
			if len(execCmd) == 1 && strings.TrimSpace(execCmd[0]) == "" {
				execCmd = []string{"/bin/sh"}
			}
			return runConnect(opts, ns, sn, execCmd, !noTTY)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "/bin/bash", "Shell to start")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Command string to execute")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Disable TTY allocation")
	return cmd
}
