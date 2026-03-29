package cli

import (
	"fmt"

	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newUseCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:               "use <session>",
		Short:             "Set active session for current repo",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, err := session.Resolve(args[0], "")
			if err != nil {
				return err
			}
			if err := session.SaveActiveSession(name); err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Active session set to %s\n", name)
			return nil
		},
	}
}
