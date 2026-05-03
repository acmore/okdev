package cli

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/acmore/okdev/internal/logx"
	"github.com/spf13/cobra"
)

type Options struct {
	ConfigPath string
	Session    string
	Owner      string
	Namespace  string
	Context    string
	Output     string
	Verbose    bool
}

func NewRootCmd() *cobra.Command {
	cmd, _ := newRootCmdWithOptions()
	return cmd
}

func newRootCmdWithOptions() (*cobra.Command, *Options) {
	opts := &Options{}

	cmd := &cobra.Command{
		Use:           "okdev",
		Short:         "Kubernetes-native developer environments",
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, args []string) {
			logx.Configure(opts.Verbose)
		},
	}

	cmd.PersistentFlags().StringVarP(&opts.ConfigPath, "config", "c", "", "Path to config file")
	cmd.PersistentFlags().StringVar(&opts.Session, "session", "", "Session name")
	cmd.PersistentFlags().StringVar(&opts.Owner, "owner", "", "Session owner identity override")
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
	cmd.AddCommand(newTargetCmd(opts))
	cmd.AddCommand(newAgentCmd(opts))
	cmd.AddCommand(newExecCmd(opts))
	cmd.AddCommand(newExecJobsCmd(opts))
	cmd.AddCommand(newPortForwardCmd(opts))
	cmd.AddCommand(newCpCmd(opts))
	cmd.AddCommand(newLogsCmd(opts))
	cmd.AddCommand(newSSHCmd(opts))
	cmd.AddCommand(newSSHProxyCmd(opts))
	cmd.AddCommand(newPortsCmd(opts))
	cmd.AddCommand(newSyncCmd(opts))
	cmd.AddCommand(newPruneCmd(opts))
	cmd.AddCommand(newMigrateCmd(opts))
	cmd.AddCommand(newTemplateCmd(opts))
	cmd.AddCommand(newUpgradeCmd())
	initRootCompletion(cmd)

	return cmd, opts
}

func Execute() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	cmd, _ := newRootCmdWithOptions()
	return executeWithContext(ctx, cmd)
}

func executeWithContext(ctx context.Context, cmd *cobra.Command) error {
	return cmd.ExecuteContext(ctx)
}
