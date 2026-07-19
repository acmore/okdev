package cli

import (
	"context"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"

	syncengine "github.com/acmore/okdev/internal/sync"
)

const syncWaitDefaultTimeout = 10 * time.Minute

func newSyncWaitCmd(opts *Options) *cobra.Command {
	var timeout time.Duration

	cmd := &cobra.Command{
		Use:   "wait [session]",
		Short: "Wait until sync has converged in both directions",
		Long: `Block until every configured sync mapping has no pending bytes in either
direction, then return. Purely a wait — it does not start or repair sync
(use "okdev sync" for that). The edit-run loop guarantee:

  vim train.py && okdev sync wait && okdev exec -- python train.py`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			status, reason := checkSyncHealth(cc.sessionName)
			if err := syncWaitGateError(cc.sessionName, status, reason); err != nil {
				return err
			}
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			return runSyncWaitConvergence(cmd.Context(), cc, target.PodName, pairs, timeout, cmd.OutOrStdout())
		},
	}

	cmd.Flags().DurationVar(&timeout, "timeout", syncWaitDefaultTimeout, "Give up if sync has not converged within this duration")
	return cmd
}

// detachSyncWarning returns a stderr warning for launching a detached job
// over an unhealthy sync channel (issue #165): the job would run stale or
// missing code (typically exit 127) with nothing tying the failure back to
// sync. Empty when the channel is healthy.
func detachSyncWarning(status syncHealthStatus, reason string) string {
	switch status {
	case syncHealthActive:
		return ""
	case syncHealthPaused:
		return "warning: sync is paused (`okdev sync pause`) — the detached job may run stale code; resume with \"okdev sync resume\" before launching, or gate the launch with --require-sync"
	case syncHealthStale:
		return fmt.Sprintf("warning: sync is running but unhealthy (%s) — the detached job may run stale code; repair with \"okdev sync\" or gate the launch with --require-sync", reason)
	default:
		return "warning: sync is not running — the detached job may run stale code; start it with \"okdev sync\" or gate the launch with --require-sync"
	}
}

// detachSyncPreflight guards a detached launch against the issue #165
// wasted-run. Without --require-sync an unhealthy channel produces a stderr
// warning only; with it, the launch aborts unless sync is healthy AND every
// mapping has fully converged. Sessions with no sync mappings are exempt from
// the warning (there is no channel to be stale).
func detachSyncPreflight(cmd *cobra.Command, cc *commandContext, requireSync bool) error {
	if len(cc.cfg.Spec.Sync.Paths) == 0 {
		if requireSync {
			return fmt.Errorf("--require-sync: session %s has no sync mappings", cc.sessionName)
		}
		return nil
	}
	status, reason := checkSyncHealth(cc.sessionName)
	if !requireSync {
		if warn := detachSyncWarning(status, reason); warn != "" {
			fmt.Fprintln(cmd.ErrOrStderr(), warn)
		}
		return nil
	}
	if err := syncWaitGateError(cc.sessionName, status, reason); err != nil {
		return fmt.Errorf("--require-sync: %w", err)
	}
	pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
	if err != nil {
		return err
	}
	target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
	if err != nil {
		return err
	}
	fmt.Fprintln(cmd.ErrOrStderr(), "--require-sync: waiting for sync convergence before launch")
	if err := runSyncWaitConvergence(cmd.Context(), cc, target.PodName, pairs, syncWaitDefaultTimeout, cmd.ErrOrStderr()); err != nil {
		return fmt.Errorf("--require-sync: %w", err)
	}
	return nil
}

// syncWaitGateError converts a session's sync health into the gate error for
// commands that need a live sync channel (sync wait, exec --require-sync).
// Issue #165: a stale channel (process alive, peer disconnected — the state a
// laptop network drop leaves behind) must never be reported as "not running";
// the two states have different repairs and the message must match what
// "okdev sync" reports for the same channel.
func syncWaitGateError(sessionName string, status syncHealthStatus, reason string) error {
	switch status {
	case syncHealthActive:
		return nil
	case syncHealthPaused:
		// Paused is intentional, not broken: pending changes cannot converge
		// by design, so waiting would only time out. Point at resume, never
		// at repair (#174).
		return fmt.Errorf("sync is paused for session %s; pending changes will not propagate until `okdev sync resume`", sessionName)
	case syncHealthStale:
		return fmt.Errorf("background sync is running but unhealthy for session %s (%s); repair it with \"okdev sync\" or \"okdev sync --reset\"", sessionName, reason)
	default:
		return fmt.Errorf("background sync is not running for session %s; start it with \"okdev sync\" or \"okdev up\" first", sessionName)
	}
}

// runSyncWaitConvergence polls every managed folder on both syncthing
// instances until local and remote pending bytes reach zero, or the timeout
// expires.
func runSyncWaitConvergence(ctx context.Context, cc *commandContext, pod string, pairs []syncengine.Pair, timeout time.Duration, out io.Writer) error {
	folders, err := resolveSyncFolders(cc.sessionName, "", pairs)
	if err != nil {
		return err
	}

	localHome, err := localSyncthingStatusHome(cc.sessionName)
	if err != nil {
		return fmt.Errorf("resolve local syncthing home: %w", err)
	}
	localBase, localKey, err := readLocalSyncthingEndpoint(localHome)
	if err != nil {
		return fmt.Errorf("read local syncthing endpoint: %w", err)
	}
	remoteKey, err := readRemoteSyncthingAPIKey(ctx, cc.kube, cc.namespace, pod)
	if err != nil {
		return fmt.Errorf("read remote syncthing API key: %w", err)
	}
	cancelPF, remoteBase, _, err := startSyncthingPortForward(ctx, cc.opts, cc.namespace, pod)
	if err != nil {
		return fmt.Errorf("port-forward to syncthing sidecar: %w", err)
	}
	defer cancelPF()
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("remote syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(ctx, localBase, localKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("local syncthing not ready: %w", err)
	}
	localID, err := syncthingDeviceID(ctx, localBase, localKey)
	if err != nil {
		return fmt.Errorf("read local syncthing device id: %w", err)
	}
	remoteID, err := syncthingDeviceID(ctx, remoteBase, remoteKey)
	if err != nil {
		return fmt.Errorf("read remote syncthing device id: %w", err)
	}

	// Force an immediate rescan on both sides so files written moments before
	// "sync wait" are indexed now rather than after the FS-watcher delay —
	// otherwise the loop below could observe convergence before the new file
	// is even known to syncthing, voiding the edit-run guarantee.
	for _, folder := range folders {
		if err := syncthingScanFolder(ctx, localBase, localKey, folder.id); err != nil {
			return fmt.Errorf("trigger local rescan of %s: %w", folder.id, err)
		}
		if err := syncthingScanFolder(ctx, remoteBase, remoteKey, folder.id); err != nil {
			return fmt.Errorf("trigger remote rescan of %s: %w", folder.id, err)
		}
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	lastReported := int64(-1)
	for {
		converged := true
		var totalNeed int64
		for _, folder := range folders {
			localPct, localNeed, localErr := syncthingCompletion(ctx, localBase, localKey, folder.id, remoteID)
			remotePct, remoteNeed, remoteErr := syncthingCompletion(ctx, remoteBase, remoteKey, folder.id, localID)
			if localErr != nil || remoteErr != nil {
				converged = false
				continue
			}
			totalNeed += localNeed + remoteNeed
			if !syncthingInitialSyncComplete(localPct, localNeed, remotePct, remoteNeed) {
				converged = false
			}
		}
		if converged {
			if len(folders) == 1 {
				fmt.Fprintln(out, "Sync converged.")
			} else {
				fmt.Fprintf(out, "Sync converged (%d folders).\n", len(folders))
			}
			return nil
		}
		if totalNeed != lastReported {
			fmt.Fprintf(out, "waiting: %s pending\n", formatSyncthingMiB(totalNeed))
			lastReported = totalNeed
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sync did not converge within %s (%s still pending); check `okdev status --details`", timeout, formatSyncthingMiB(totalNeed))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
