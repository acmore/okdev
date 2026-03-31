package cli

import (
	"os"
	"time"

	"github.com/acmore/okdev/internal/analytics"
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
		Use:   "okdev",
		Short: "Kubernetes-native developer environments",
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
	cmd.AddCommand(newLogsCmd(opts))
	cmd.AddCommand(newSSHCmd(opts))
	cmd.AddCommand(newSSHProxyCmd(opts))
	cmd.AddCommand(newPortsCmd(opts))
	cmd.AddCommand(newSyncCmd(opts))
	cmd.AddCommand(newPruneCmd(opts))
	cmd.AddCommand(newMigrateCmd(opts))
	initRootCompletion(cmd)

	return cmd, opts
}

func Execute() error {
	cmd, opts := newRootCmdWithOptions()
	collector := analytics.NewFromEnv()
	commandPath := resolvedCommandPath(cmd, os.Args[1:])
	start := time.Now()
	err := cmd.Execute()
	if collector != nil {
		collector.TrackCommand(commandPath, opts.Output, opts.Verbose, err, time.Since(start))
	}
	return err
}

func resolvedCommandPath(root *cobra.Command, args []string) string {
	if root == nil {
		return "okdev"
	}
	target, _, err := root.Find(args)
	if err != nil || target == nil {
		return root.CommandPath()
	}
	return target.CommandPath()
}
