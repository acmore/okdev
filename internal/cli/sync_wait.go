package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
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
		Long: `Rescan and wait until every mapping's current indexed revision is acknowledged
by the local and target devices, with no pending items or deletions. Ignored paths
are outside this check. Primary-mapping mesh receivers are verified too. It does not start or repair sync
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

// syncStalenessWarning returns a stderr warning for running a command over an
// unhealthy sync channel (issue #165): the command would run stale or missing
// code (typically exit 127) with nothing tying the failure back to sync. Empty
// when the channel is healthy.
//
// subject names what is at risk, because the same channel problem is reported
// on two paths now (#222): a detached job and a foreground command are equally
// capable of running pre-edit code, and a foreground experiment that does so
// produces a plausible-looking result rather than a crash.
func syncStalenessWarning(status syncHealthStatus, reason string, subject string) string {
	if strings.TrimSpace(subject) == "" {
		subject = "the command"
	}
	switch status {
	case syncHealthActive:
		return ""
	case syncHealthPaused:
		return fmt.Sprintf("warning: sync is paused (`okdev sync pause`) — %s may run stale code; resume with \"okdev sync resume\" before running, or gate it with --require-sync", subject)
	case syncHealthStale:
		return fmt.Sprintf("warning: sync is running but unhealthy (%s) — %s may run stale code; repair with \"okdev sync\" or gate it with --require-sync", reason, subject)
	default:
		return fmt.Sprintf("warning: sync is not running — %s may run stale code; start it with \"okdev sync\" or gate it with --require-sync", subject)
	}
}

// execSyncPreflight guards a run against the issue #165 wasted-run. Without
// --require-sync an unhealthy channel produces a stderr warning only; with it,
// the run aborts unless sync is healthy AND every mapping has fully converged.
// Sessions with no sync mappings are exempt from the warning (there is no
// channel to be stale).
//
// Both foreground and detached runs come through here (#222). The gate is what
// actually changes behaviour — it stands in front of the action and refuses
// with a reason — and it used to cover only the detached half of the workload,
// even though the failure being guarded is identical on both.
func execSyncPreflight(cmd *cobra.Command, cc *commandContext, requireSync bool, subject string) error {
	if len(cc.cfg.Spec.Sync.Paths) == 0 {
		if requireSync {
			return fmt.Errorf("--require-sync: session %s has no sync mappings", cc.sessionName)
		}
		return nil
	}
	status, reason := checkSyncHealth(cc.sessionName)
	if !requireSync {
		if warn := syncPreflightWarning(cc.sessionName, status, reason, subject); warn != "" {
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
	fmt.Fprintln(cmd.ErrOrStderr(), "--require-sync: waiting for sync convergence before running")
	if err := runSyncWaitConvergence(cmd.Context(), cc, target.PodName, pairs, syncWaitDefaultTimeout, cmd.ErrOrStderr()); err != nil {
		return fmt.Errorf("--require-sync: %w", err)
	}
	return nil
}

// isSyncthingScanStillRunning reports whether a scan request failed only
// because it outlived the client timeout. The scan itself continues on the
// syncthing side, so this is "not finished yet", not "did not happen".
func isSyncthingScanStillRunning(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || os.IsTimeout(err) {
		return true
	}
	// net/http wraps the client timeout in a *url.Error whose Timeout() is
	// true; the message form is checked as a last resort for wrapped errors
	// that lose the interface.
	var urlErr *url.Error
	if errors.As(err, &urlErr) && urlErr.Timeout() {
		return true
	}
	return strings.Contains(err.Error(), "Client.Timeout exceeded")
}

// syncthingFolderScanSettled requires both ends to be idle; queued scans
// and folder errors cannot establish that indexing completed.
func syncthingFolderScanSettled(ctx context.Context, localBase, localKey, remoteBase, remoteKey, folderID string) (bool, error) {
	for _, end := range []struct{ base, key string }{{localBase, localKey}, {remoteBase, remoteKey}} {
		info, err := syncthingFolderStatusInfoForFolder(ctx, end.base, end.key, folderID)
		if err != nil {
			return false, err
		}
		if !strings.EqualFold(strings.TrimSpace(info.State), "idle") {
			return false, nil
		}
	}
	return true, nil
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
func runSyncWaitConvergence(ctx context.Context, cc *commandContext, pod string, pairs []syncengine.Pair, timeout time.Duration, out io.Writer, minimumReceivers ...int) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	folders, err := resolveSyncFolders(cc.sessionName, "", pairs)
	if err != nil {
		return err
	}

	if len(folders) == 0 {
		return fmt.Errorf("no sync mappings to verify")
	}
	meshCheck, err := newMeshConvergenceCheck(ctx, cc, pod, folders[0].id, minimumReceivers...)
	if err != nil {
		return fmt.Errorf("discover intended sync receivers: %w", err)
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

	return waitSyncthingRevisions(ctx, localBase, localKey, remoteBase, remoteKey, localID, remoteID, folders, out, meshCheck)
}
