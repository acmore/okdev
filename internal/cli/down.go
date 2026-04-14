package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

var downWaitPollInterval = 200 * time.Millisecond

type downWaitOutput struct {
	Enabled         bool   `json:"enabled"`
	Timeout         string `json:"timeout,omitempty"`
	Status          string `json:"status,omitempty"`
	WorkloadDeleted bool   `json:"workloadDeleted,omitempty"`
	PodsDeleted     bool   `json:"podsDeleted,omitempty"`
}

type downOutput struct {
	Session   string          `json:"session"`
	Namespace string          `json:"namespace"`
	Kind      string          `json:"kind"`
	Workload  string          `json:"workload"`
	DryRun    bool            `json:"dryRun"`
	Deleted   bool            `json:"deleted"`
	Status    string          `json:"status"`
	Notes     []string        `json:"notes,omitempty"`
	Wait      *downWaitOutput `json:"wait,omitempty"`
	Cleanup   map[string]any  `json:"cleanup,omitempty"`
}

func newDownCmd(opts *Options) *cobra.Command {
	var deletePVC bool
	var dryRun bool
	var wait bool
	var waitTimeout time.Duration
	var yes bool

	cmd := &cobra.Command{
		Use:   "down [session]",
		Short: "Delete a dev session pod",
		Args:  cobra.MaximumNArgs(1),
		Example: `  # Delete the current session (prompts for confirmation)
  okdev down

  # Delete without confirmation (for scripts/CI)
  okdev down --yes

  # Preview what would be deleted
  okdev down --dry-run

  # Emit a machine-readable delete summary
  okdev down --output json --yes

  # Delete and wait for the workload and matching pods to disappear
  okdev down --wait

  # Delete a specific session
  okdev down my-feature -y`,
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
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
			runtime, err := sessionRuntimeForExisting(cmd.Context(), cc.cfg, cc.cfgPath, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if err := ensureSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")
			ctx, cancel := downCommandContext(wait, waitTimeout)
			defer cancel()
			exists, err := shouldReuseExistingWorkload(ctx, cc.kube, cc.namespace, runtime)
			if err != nil {
				return err
			}
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
				if wait {
					payload.Wait = &downWaitOutput{
						Enabled: true,
						Timeout: waitTimeout.String(),
						Status:  "planned",
					}
				}
				if deletePVC {
					payload.Notes = append(payload.Notes, "--delete-pvc ignored: okdev no longer manages PVC lifecycle")
				}
				if !exists {
					payload.Status = "already stopped"
					payload.Notes = append(payload.Notes, "session workload already absent")
				}
				if cc.opts.Output == "json" {
					return outputJSON(cmd.OutOrStdout(), payload)
				}
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", cc.sessionName, cc.namespace)
				if exists {
					fmt.Fprintf(cmd.OutOrStdout(), "- would delete %s/%s\n", runtime.Kind(), runtime.WorkloadName())
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "- %s/%s already absent\n", runtime.Kind(), runtime.WorkloadName())
				}
				if deletePVC {
					fmt.Fprintln(cmd.OutOrStdout(), "- note: --delete-pvc is ignored (okdev no longer manages PVC lifecycle)")
				}
				if wait {
					fmt.Fprintf(cmd.OutOrStdout(), "- would wait for workload deletion and session pods to terminate (timeout=%s)\n", waitTimeout)
				}
				return nil
			}

			if !exists {
				payload := downOutput{
					Session:   cc.sessionName,
					Namespace: cc.namespace,
					Kind:      runtime.Kind(),
					Workload:  runtime.WorkloadName(),
					DryRun:    false,
					Deleted:   false,
					Status:    "already stopped",
					Notes:     []string{"session workload already absent"},
					Cleanup:   map[string]any{},
				}
				if wait {
					payload.Wait = &downWaitOutput{
						Enabled:         true,
						Timeout:         waitTimeout.String(),
						Status:          "already stopped",
						WorkloadDeleted: true,
						PodsDeleted:     true,
					}
				}
				if err := downCleanupLocal(ui, &payload, cc.sessionName); err != nil {
					return err
				}
				if cc.opts.Output == "json" {
					return outputJSON(cmd.OutOrStdout(), payload)
				}
				ui.printWarnings()
				ui.section("Ready")
				fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", cc.sessionName)
				fmt.Fprintf(cmd.OutOrStdout(), "namespace: %s\n", cc.namespace)
				fmt.Fprintln(cmd.OutOrStdout(), "status:    already stopped")
				fmt.Fprintln(cmd.OutOrStdout(), "workspace: workload already absent; local session state cleaned")
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
			if wait {
				payload.Status = "terminating"
				payload.Wait = &downWaitOutput{
					Enabled: true,
					Timeout: waitTimeout.String(),
					Status:  "waiting",
				}
			}
			if len(cc.cfg.Spec.Agents) > 0 {
				if target, err := resolveTargetRef(ctx, cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube); err != nil {
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
			var waitErr error
			if wait {
				ui.section("Wait")
				ui.stepRun("termination", runtime.WorkloadName())
				waitResult, err := waitForDownDeletion(ctx, cc.kube, cc.namespace, cc.sessionName, runtime, waitTimeout)
				payload.Wait = &waitResult
				if err != nil {
					waitErr = err
					payload.Status = "terminating"
					ui.warnf("%v", err)
				} else {
					payload.Status = "stopped"
					ui.stepDone("termination", "workload deleted and session pods gone")
				}
			}
			if deletePVC {
				ui.warnf("--delete-pvc ignored: okdev no longer manages PVC lifecycle; delete PVCs manually if needed")
				payload.Notes = append(payload.Notes, "--delete-pvc ignored: okdev no longer manages PVC lifecycle; delete PVCs manually if needed")
			}
			ui.stepDone("pvc", "not managed")
			payload.Cleanup["pvc"] = "not managed"
			if err := downCleanupLocal(ui, &payload, cc.sessionName); err != nil {
				return err
			}
			if cc.opts.Output == "json" {
				if err := outputJSON(cmd.OutOrStdout(), payload); err != nil {
					return err
				}
				return waitErr
			}
			if waitErr != nil {
				ui.printWarnings()
				return waitErr
			}
			ui.printWarnings()
			ui.section("Ready")
			fmt.Fprintf(cmd.OutOrStdout(), "session:   %s\n", cc.sessionName)
			fmt.Fprintf(cmd.OutOrStdout(), "namespace: %s\n", cc.namespace)
			fmt.Fprintln(cmd.OutOrStdout(), "status:    stopped")
			if wait {
				fmt.Fprintln(cmd.OutOrStdout(), "workspace: workload deleted and session pods terminated; volumes/PVCs unchanged")
			} else {
				fmt.Fprintln(cmd.OutOrStdout(), "workspace: pod deleted; volumes/PVCs unchanged")
			}
			return nil
		},
	}
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "Skip confirmation prompt")
	cmd.Flags().BoolVar(&deletePVC, "delete-pvc", false, "Delete workspace PVC for this session")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without deleting resources")
	cmd.Flags().BoolVar(&wait, "wait", false, "Wait for workload deletion and session pod termination")
	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", downDefaultWaitTimeout, "Wait timeout for workload termination")
	_ = cmd.Flags().MarkDeprecated("delete-pvc", "PVC lifecycle is no longer managed; delete PVCs manually if needed")
	return cmd
}

type downWaitClient interface {
	ResourceExists(context.Context, string, string, string, string) (bool, error)
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
}

func downCommandContext(wait bool, waitTimeout time.Duration) (context.Context, context.CancelFunc) {
	timeout := defaultContextTimeout
	if wait {
		needed := waitTimeout + downContextBuffer
		if needed > timeout {
			timeout = needed
		}
	}
	return context.WithTimeout(context.Background(), timeout)
}

func waitForDownDeletion(ctx context.Context, k downWaitClient, namespace, sessionName string, runtime workload.Runtime, timeout time.Duration) (downWaitOutput, error) {
	result := downWaitOutput{
		Enabled: true,
		Timeout: timeout.String(),
		Status:  "waiting",
	}
	ref, ok := runtime.(workload.RefProvider)
	if !ok {
		return result, fmt.Errorf("wait for %s deletion is unsupported", runtime.Kind())
	}
	apiVersion, kind, name, err := ref.WorkloadRef()
	if err != nil {
		return result, fmt.Errorf("resolve workload reference for wait: %w", err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	if err := waitForResourceDeletion(waitCtx, k, namespace, apiVersion, kind, name); err != nil {
		result.Status = "timed out"
		return result, err
	}
	result.WorkloadDeleted = true
	if err := waitForSessionPodsDeleted(waitCtx, k, namespace, sessionName); err != nil {
		result.Status = "timed out"
		return result, err
	}
	result.PodsDeleted = true
	result.Status = "completed"
	return result, nil
}

func waitForResourceDeletion(ctx context.Context, k downWaitClient, namespace, apiVersion, kind, name string) error {
	timeoutMessage := fmt.Sprintf("timed out waiting for %s/%s to be deleted", kind, name)
	ticker := time.NewTicker(downWaitPollInterval)
	defer ticker.Stop()
	for {
		exists, err := k.ResourceExists(ctx, namespace, apiVersion, kind, name)
		if err != nil {
			if ctx.Err() != nil {
				return downWaitContextError(ctx, timeoutMessage)
			}
			return fmt.Errorf("check %s/%s deletion: %w", kind, name, err)
		}
		if !exists {
			return nil
		}
		select {
		case <-ctx.Done():
			return downWaitContextError(ctx, timeoutMessage)
		case <-ticker.C:
		}
	}
}

func waitForSessionPodsDeleted(ctx context.Context, k downWaitClient, namespace, sessionName string) error {
	ticker := time.NewTicker(downWaitPollInterval)
	defer ticker.Stop()
	selector := "okdev.io/managed=true,okdev.io/session=" + sessionName
	for {
		pods, err := k.ListPods(ctx, namespace, false, selector)
		if err != nil {
			if ctx.Err() != nil {
				return downWaitContextError(ctx, "timed out waiting for session pods to terminate")
			}
			return fmt.Errorf("list session pods while waiting for deletion: %w", err)
		}
		if len(pods) == 0 {
			return nil
		}
		select {
		case <-ctx.Done():
			names := make([]string, 0, len(pods))
			for _, pod := range pods {
				names = append(names, pod.Name)
			}
			return downWaitContextError(ctx, fmt.Sprintf("timed out waiting for session pods to terminate: %s", strings.Join(names, ", ")))
		case <-ticker.C:
		}
	}
}

func downWaitContextError(ctx context.Context, timeoutMessage string) error {
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		return errors.New(timeoutMessage)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	return errors.New(timeoutMessage)
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

func downCleanupLocal(ui *upUI, payload *downOutput, sessionName string) error {
	if ui == nil || payload == nil {
		return nil
	}
	if payload.Cleanup == nil {
		payload.Cleanup = map[string]any{}
	}
	alias := sshHostAlias(sessionName)
	ui.section("Cleanup")
	if err := session.RequestShutdown(sessionName); err != nil {
		ui.warnf("failed to request shutdown for local clients: %v", err)
		payload.Cleanup["localClients"] = "warning"
	} else {
		ui.stepDone("local clients", "shutdown requested")
		payload.Cleanup["localClients"] = "shutdown requested"
	}
	if err := stopDetachedSyncthingSync(sessionName); err != nil {
		ui.warnf("failed to stop background sync: %v", err)
		payload.Cleanup["sync"] = "warning"
	} else {
		ui.stepDone("sync", "stopped")
		payload.Cleanup["sync"] = "stopped"
	}
	if err := stopLocalSyncthingForSession(sessionName); err != nil {
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
	if active, err := session.LoadActiveSession(); err == nil && active == sessionName {
		if clearErr := session.ClearActiveSession(); clearErr != nil {
			ui.warnf("failed to clear active session: %v", clearErr)
			payload.Cleanup["activeSession"] = "warning"
		} else {
			ui.stepDone("active session", "cleared")
			payload.Cleanup["activeSession"] = "cleared"
		}
	}
	if err := session.ClearTarget(sessionName); err != nil {
		ui.warnf("failed to clear target state: %v", err)
		payload.Cleanup["target"] = "warning"
	} else {
		ui.stepDone("target", "cleared")
		payload.Cleanup["target"] = "cleared"
	}
	if err := saveSyncthingTargetSessionState(sessionName, syncthingSessionTargetState{}); err != nil {
		ui.warnf("failed to clear syncthing sync state: %v", err)
		payload.Cleanup["syncState"] = "warning"
	} else {
		ui.stepDone("sync state", "cleared")
		payload.Cleanup["syncState"] = "cleared"
	}
	return nil
}
