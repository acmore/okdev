package cli

import (
	"fmt"
	"os"
	"strconv"
	"time"

	"github.com/spf13/cobra"
)

func newPortsCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "ports",
		Short: "Start port-forwarding for configured ports",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			if err := ensureSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout()); err != nil {
				return err
			}
			if len(cfg.Spec.Ports) == 0 {
				return fmt.Errorf("no ports configured in config")
			}

			forwards := make([]string, 0, len(cfg.Spec.Ports))
			for _, p := range cfg.Spec.Ports {
				if p.Local <= 0 || p.Remote <= 0 {
					continue
				}
				forwards = append(forwards, strconv.Itoa(p.Local)+":"+strconv.Itoa(p.Remote))
			}
			if len(forwards) == 0 {
				return fmt.Errorf("no valid ports configured")
			}

			ctx, cancel := interactiveContext()
			defer cancel()
			fmt.Fprintf(cmd.OutOrStdout(), "Forwarding %v from session %s\n", forwards, sn)
			k := newKubeClient(opts)
			backoff := 1 * time.Second
			for {
				err := k.PortForward(ctx, ns, podName(sn), forwards, os.Stdout, os.Stderr)
				if err == nil {
					return nil
				}
				fmt.Fprintf(cmd.ErrOrStderr(), "port-forward error: %v (retrying in %s)\n", err, backoff)
				time.Sleep(backoff)
				if backoff < 30*time.Second {
					backoff *= 2
					if backoff > 30*time.Second {
						backoff = 30 * time.Second
					}
				}
			}
		},
	}
}
