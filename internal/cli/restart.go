package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

func newRestartCmd(opts *Options) *cobra.Command {
	var yes bool
	var waitTimeout time.Duration

	cmd := &cobra.Command{
		Use:   "restart [session]",
		Short: "Recreate the session workload with the same config",
		Long: `Delete the session workload, wait for it to terminate, and bring it back
up from the current config in one command — the recovery path when a
container died and the restart policy will not bring it back.

PVCs and other resources referenced by spec.volumes are untouched.
Lifecycle hooks (postCreate/postSync) re-run automatically on the fresh
pods, and sync re-bootstraps against the new workload.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		Example: `  # Restart the current session (prompts for confirmation)
  okdev restart

  # Restart without confirmation
  okdev restart --yes

  # Restart a specific session
  okdev restart my-feature -y`,
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
			ui.section("Validate")
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			ui.stepDone("config", cc.cfgPath)
			ui.stepDone("session", cc.sessionName)
			ui.stepDone("namespace", cc.namespace)
			runtime, err := sessionRuntimeForExisting(cmd.Context(), cc.cfg, cc.cfgPath, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if err := ensureSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")

			delCtx, cancel := downCommandContext(true, waitTimeout)
			defer cancel()
			exists, err := shouldReuseExistingWorkload(delCtx, cc.kube, cc.namespace, runtime)
			if err != nil {
				return err
			}

			if exists {
				if !yes {
					ok, err := confirmRestart(os.Stdin, cmd.ErrOrStderr(), cc.sessionName, cc.namespace, runtime.Kind(), runtime.WorkloadName())
					if err != nil {
						return err
					}
					if !ok {
						fmt.Fprintln(cmd.OutOrStdout(), "Aborted.")
						return nil
					}
				}
				ui.section("Delete")
				if err := runtime.Delete(delCtx, cc.kube, cc.namespace, true); err != nil {
					return fmt.Errorf("delete workload: %w", err)
				}
				ui.stepRun("workload", "waiting for deletion")
				if _, err := waitForDownDeletion(delCtx, cc.kube, cc.namespace, cc.sessionName, runtime, waitTimeout); err != nil {
					return err
				}
				ui.stepDone("workload", fmt.Sprintf("%s/%s deleted", runtime.Kind(), runtime.WorkloadName()))
			} else {
				ui.stepDone("workload", "already absent")
			}

			// Reset local per-session state (sync daemons, pins) so the up
			// below bootstraps cleanly against the fresh workload.
			payload := downOutput{Cleanup: map[string]any{}}
			if err := downCleanupLocal(ui, &payload, cc.sessionName); err != nil {
				return err
			}

			return runUp(cmd, opts, upOptions{waitTimeout: waitTimeout})
		},
	}

	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip the confirmation prompt")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", upDefaultWaitTimeout, "Timeout for workload deletion and pod readiness")
	return cmd
}

func confirmRestart(in io.Reader, out io.Writer, sessionName, namespace, kind, workloadName string) (bool, error) {
	if !isTerminalReader(in) {
		return false, fmt.Errorf("refusing to restart without --yes in non-interactive mode")
	}
	fmt.Fprintf(out, "Restart session %q in namespace %q? This deletes and recreates %s/%s. [y/N]: ", sessionName, namespace, kind, workloadName)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
