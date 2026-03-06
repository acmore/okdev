package cli

import (
	"github.com/acmore/okdev/internal/logx"
	"github.com/spf13/cobra"
)

type Options struct {
	ConfigPath string
	Session    string
	Namespace  string
	Context    string
	Output     string
	Verbose    bool
}

func NewRootCmd() *cobra.Command {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:   "okdev",
		Short: "Kubernetes-native developer environments",
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			logx.Configure(opts.Verbose)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.ConfigPath, "config", "c", "", "Path to config file")
	cmd.PersistentFlags().StringVar(&opts.Session, "session", "", "Session name")
	cmd.PersistentFlags().StringVarP(&opts.Namespace, "namespace", "n", "", "Kubernetes namespace override")
	cmd.PersistentFlags().StringVar(&opts.Context, "context", "", "Kubernetes context override")
	cmd.PersistentFlags().StringVar(&opts.Output, "output", "text", "Output format: text|json")
	cmd.PersistentFlags().BoolVar(&opts.Verbose, "verbose", false, "Enable verbose logging")

	cmd.AddCommand(newVersionCmd())
	cmd.AddCommand(newInitCmd(opts))
	cmd.AddCommand(newValidateCmd(opts))
	cmd.AddCommand(newUpCmd(opts))
	cmd.AddCommand(newDownCmd(opts))
	cmd.AddCommand(newStatusCmd(opts))
	cmd.AddCommand(newListCmd(opts))
	cmd.AddCommand(newUseCmd(opts))
	cmd.AddCommand(newConnectCmd(opts))
	cmd.AddCommand(newSSHCmd(opts))
	cmd.AddCommand(newPortsCmd(opts))
	cmd.AddCommand(newSyncCmd(opts))
	cmd.AddCommand(newPruneCmd(opts))

	return cmd
}

func Execute() error {
	return NewRootCmd().Execute()
}
