package cli

import (
	"github.com/spf13/cobra"
)

type Options struct {
	ConfigPath string
}

func NewRootCmd() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "okdev",
		Short: "Kubernetes-native developer environments",
	}

	cmd.PersistentFlags().StringVarP(&opts.ConfigPath, "config", "c", "", "Path to config file")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newInitCmd(opts))
	cmd.AddCommand(newValidateCmd(opts))

	return cmd
}

func Execute() error {
	return NewRootCmd().Execute()
}
