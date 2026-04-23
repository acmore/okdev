package cli

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/spf13/cobra"
)

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

func parsePortForwardMappings(args []string) ([]string, error) {
	if len(args) == 0 {
		return nil, fmt.Errorf("requires at least one LOCAL:REMOTE mapping")
	}
	out := make([]string, 0, len(args))
	for _, arg := range args {
		parts := strings.Split(arg, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid port mapping %q, expected LOCAL:REMOTE", arg)
		}
		local, err := strconv.Atoi(parts[0])
		if err != nil || local <= 0 {
			return nil, fmt.Errorf("invalid local port in mapping %q", arg)
		}
		remote, err := strconv.Atoi(parts[1])
		if err != nil || remote <= 0 {
			return nil, fmt.Errorf("invalid remote port in mapping %q", arg)
		}
		out = append(out, fmt.Sprintf("%d:%d", local, remote))
	}
	return out, nil
}
