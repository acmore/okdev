package cli

import (
	"bufio"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

type downOutput struct {
	Session   string         `json:"session"`
	Namespace string         `json:"namespace"`
	Kind      string         `json:"kind"`
	Workload  string         `json:"workload"`
	DryRun    bool           `json:"dryRun"`
	Deleted   bool           `json:"deleted"`
	Status    string         `json:"status"`
	Notes     []string       `json:"notes,omitempty"`
	Cleanup   map[string]any `json:"cleanup,omitempty"`
}

func newDownCmd(opts *Options) *cobra.Command {
	var deletePVC bool
	var dryRun bool
	var yes bool

	cmd := &cobra.Command{
		Use:   "down",
		Short: "Delete a dev session pod",
		Example: `  # Delete the current session (prompts for confirmation)
  okdev down

  # Delete without confirmation (for scripts/CI)
  okdev down --yes

  # Preview what would be deleted
  okdev down --dry-run

  # Emit a machine-readable delete summary
  okdev down --output json --yes

  # Delete a specific session
  okdev down --session my-feature -y`,
		RunE: func(cmd *cobra.Command, args []string) error {
			uiOut := cmd.OutOrStdout()
			uiErr := cmd.ErrOrStderr()
			if opts.Output == "json" {
				uiOut = io.Discard
				uiErr = io.Discard
			}
			ui := newUpUI(uiOut, uiErr)
			ui.section("Validate")
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			ui.stepDone("config", cc.cfgPath)
			ui.stepDone("session", cc.sessionName)
			ui.stepDone("namespace", cc.namespace)
			runtime, err := sessionRuntimeForExisting(cc.cfg, cc.cfgPath, cc.sessionName)
			if err != nil {
				return err
			}
			if err := ensureSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, false); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")
			ctx, cancel := defaultContext()
			defer cancel()
			if dryRun {
				payload := downOutput{
					Session:   cc.sessionName,
					Namespace: cc.namespace,
					Kind:      runtime.Kind(),
					Workload:  runtime.WorkloadName(),
					DryRun:    true,
					Deleted:   false,
					Status:    "planned",
				}
				if deletePVC {
					payload.Notes = append(payload.Notes, "--delete-pvc ignored: okdev no longer manages PVC lifecycle")
				}
				if opts.Output == "json" {
					return outputJSON(cmd.OutOrStdout(), payload)
				}
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", cc.sessionName, cc.namespace)
				fmt.Fprintf(cmd.OutOrStdout(), "- would delete %s/%s\n", runtime.Kind(), runtime.WorkloadName())
				if deletePVC {
					fmt.Fprintln(cmd.OutOrStdout(), "- note: --delete-pvc is ignored (okdev no longer manages PVC lifecycle)")
				}
				return nil
			}

			if !yes {
				ok, err := confirmDown(os.Stdin, cmd.ErrOrStderr(), cc.sessionName, cc.namespace, runtime.Kind(), runtime.WorkloadName())
				if err != nil {
					return err
				}
				if !ok {
					fmt.Fprintln(cmd.ErrOrStderr(), "aborted")
					return nil
				}
			}

			payload := downOutput{
				Session:   cc.sessionName,
				Namespace: cc.namespace,
				Kind:      runtime.Kind(),
				Workload:  runtime.WorkloadName(),
				DryRun:    false,
				Deleted:   true,
				Status:    "stopped",
				Cleanup:   map[string]any{},
			}
			if len(cc.cfg.Spec.Agents) > 0 {
				if target, err := resolveTargetRef(ctx, opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube); err != nil {
					ui.warnf("failed to resolve target before agent auth cleanup: %v", err)
					payload.Cleanup["agentAuth"] = "warning"
				} else {
					results := cleanupConfiguredAgentAuth(ctx, cc.kube, cc.namespace, target.PodName, target.Container, cc.cfg.Spec.Agents, ui.warnf)
					if len(results) > 0 {
						ui.stepDone("agent auth", strings.Join(results, ", "))
						payload.Cleanup["agentAuth"] = results
					}
				}
			}

			ui.section("Delete")
			ui.stepRun(runtime.Kind(), runtime.WorkloadName())
			if err := runtime.Delete(ctx, cc.kube, cc.namespace, true); err != nil {
				return fmt.Errorf("delete session %s: %w", runtime.Kind(), err)
			}
			ui.stepDone(runtime.Kind(), "deleted")
			if deletePVC {
				ui.warnf("--delete-pvc ignored: okdev no longer manages PVC lifecycle; delete PVCs manually if needed")
				payload.Notes = append(payload.Notes, "--delete-pvc ignored: okdev no longer manages PVC lifecycle; delete PVCs manually if needed")
			}
			ui.stepDone("pvc", "not managed")
			payload.Cleanup["pvc"] = "not managed"
			alias := sshHostAlias(cc.sessionName)
			ui.section("Cleanup")
			if err := session.RequestShutdown(cc.sessionName); err != nil {
				ui.warnf("failed to request shutdown for local clients: %v", err)
				payload.Cleanup["localClients"] = "warning"
			} else {
				ui.stepDone("local clients", "shutdown requested")
				payload.Cleanup["localClients"] = "shutdown requested"
			}
			if err := stopDetachedSyncthingSync(cc.sessionName); err != nil {
				ui.warnf("failed to stop background sync: %v", err)
				payload.Cleanup["sync"] = "warning"
			} else {
				ui.stepDone("sync", "stopped")
				payload.Cleanup["sync"] = "stopped"
			}
			if err := stopLocalSyncthingForSession(cc.sessionName); err != nil {
				ui.warnf("failed to stop local syncthing: %v", err)
				payload.Cleanup["syncthing"] = "warning"
			} else {
				ui.stepDone("syncthing", "stopped")
				payload.Cleanup["syncthing"] = "stopped"
			}
			if err := stopManagedSSHForward(alias); err != nil {
				slog.Debug("failed to stop managed SSH forward", "alias", alias, "error", err)
				payload.Cleanup["sshForward"] = "warning"
			} else {
				payload.Cleanup["sshForward"] = "stopped"
			}
			ui.stepDone("ssh forward", "stopped")
			if err := removeSSHConfigEntry(alias); err != nil {
				ui.warnf("failed to remove SSH config entry for %s: %v", alias, err)
				payload.Cleanup["sshConfig"] = "warning"
			} else {
				ui.stepDone("ssh config", "cleaned")
				payload.Cleanup["sshConfig"] = "cleaned"
			}
			if active, err := session.LoadActiveSession(); err == nil && active == cc.sessionName {
				if clearErr := session.ClearActiveSession(); clearErr != nil {
					ui.warnf("failed to clear active session: %v", clearErr)
					payload.Cleanup["activeSession"] = "warning"
				} else {
					ui.stepDone("active session", "cleared")
					payload.Cleanup["activeSession"] = "cleared"
				}
			}
			if err := session.ClearTarget(cc.sessionName); err != nil {
				ui.warnf("failed to clear target state: %v", err)
				payload.Cleanup["target"] = "warning"
			} else {
				ui.stepDone("target", "cleared")
				payload.Cleanup["target"] = "cleared"
			}
			if opts.Output == "json" {
				return outputJSON(cmd.OutOrStdout(), payload)
			}
			ui.printWarnings()
			ui.section("Ready")
			fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", cc.sessionName)
			fmt.Fprintf(cmd.OutOrStdout(), "namespace: %s\n", cc.namespace)
			fmt.Fprintln(cmd.OutOrStdout(), "status:    stopped")
			fmt.Fprintln(cmd.OutOrStdout(), "workspace: pod deleted; volumes/PVCs unchanged")
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&deletePVC, "delete-pvc", false, "Delete workspace PVC for this session")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without deleting resources")
	_ = cmd.Flags().MarkDeprecated("delete-pvc", "PVC lifecycle is no longer managed; delete PVCs manually if needed")
	return cmd
}

func confirmDown(in io.Reader, out io.Writer, sessionName, namespace, kind, workloadName string) (bool, error) {
	if !isTerminalReader(in) {
		return false, fmt.Errorf("refusing to delete without --yes in non-interactive mode")
	}
	return promptConfirmDown(in, out, sessionName, namespace, kind, workloadName)
}

func promptConfirmDown(in io.Reader, out io.Writer, sessionName, namespace, kind, workloadName string) (bool, error) {
	fmt.Fprintf(out, "Delete session %q in namespace %q? (%s/%s) [y/N]: ", sessionName, namespace, kind, workloadName)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}

func removeSSHConfigEntry(hostAlias string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	configPath := filepath.Join(home, ".ssh", "config")
	begin := "# BEGIN OKDEV " + hostAlias
	end := "# END OKDEV " + hostAlias
	if _, err := os.Stat(configPath); err != nil {
		return nil
	}
	return updateSSHConfigWithLock(configPath, func(existing string) string {
		updated := strings.TrimSpace(stripManagedSSHBlock(existing, begin, end))
		if updated == "" {
			return ""
		}
		return updated + "\n"
	})
}
