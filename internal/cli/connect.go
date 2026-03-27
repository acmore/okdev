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
		Use:   "connect [session]",
		Short: "Open shell or run command in session pod",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, true); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			stopMaintenance := startSessionMaintenance(opts, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			execCmd := []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			if shell != "" {
				execCmd = []string{shell}
			}
			if cmdStr != "" {
				execCmd = []string{"sh", "-lc", cmdStr}
			}
			if len(args) > 0 {
				execCmd = args
			}
			if len(execCmd) == 1 && strings.TrimSpace(execCmd[0]) == "" {
				execCmd = []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			}
			return runConnectWithClient(cc.kube, cc.namespace, target, execCmd, !noTTY)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to start (default auto-detects bash/sh)")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Command string to execute")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Disable TTY allocation")
	return cmd
}
