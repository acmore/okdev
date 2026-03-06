package cli

import (
	"fmt"
	"log/slog"
	"os"
	"strconv"
	"time"

	portsrunner "github.com/acmore/okdev/internal/ports"
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
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			stopRenew, err := acquireSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			defer stopRenew()
			stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), true, true)
			defer stopMaintenance()
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
			slog.Debug("ports start", "namespace", ns, "session", sn, "forwards", forwards)
			return portsrunner.ForwardWithRetry(ctx, k, ns, podName(sn), forwards, os.Stdout, cmd.ErrOrStderr(), 30*time.Second)
		},
	}
}
