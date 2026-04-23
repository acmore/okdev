package cli

import "github.com/spf13/cobra"

func newPortForwardCmd(opts *Options) *cobra.Command {
	var podName string
	var role string
	var container string
	var readyOnly bool

	cmd := &cobra.Command{
		Use:   "port-forward [session] <local:remote>...",
		Short: "Run a direct foreground pod port-forward",
		RunE: func(cmd *cobra.Command, args []string) error {
			return nil
		},
	}
	cmd.Flags().StringVar(&podName, "pod", "", "Target a specific pod by name")
	cmd.Flags().StringVar(&role, "role", "", "Target a pod by workload role")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Use only already-running pods")
	return cmd
}
