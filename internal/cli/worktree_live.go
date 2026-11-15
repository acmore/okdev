package cli

import (
	"context"
	"fmt"
	"time"

	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
)

// liveSync addresses the two Syncthing daemons of a session whose sync is
// already running: the local one through its recorded endpoint, the sidecar's
// through a port-forward of its own.
type liveSync struct {
	localBase, localKey, localID    string
	remoteBase, remoteKey, remoteID string
	close                           func()
}

func openLiveSync(ctx context.Context, cc *commandContext, pod string) (*liveSync, error) {
	localHome, err := localSyncthingStatusHome(cc.sessionName)
	if err != nil {
		return nil, fmt.Errorf("resolve local syncthing home: %w", err)
	}
	live := &liveSync{}
	live.localBase, live.localKey, err = readLocalSyncthingEndpoint(localHome)
	if err != nil {
		return nil, fmt.Errorf("read local syncthing endpoint: %w", err)
	}
	live.remoteKey, err = readRemoteSyncthingAPIKey(ctx, cc.kube, cc.namespace, pod)
	if err != nil {
		return nil, fmt.Errorf("read remote syncthing API key: %w", err)
	}
	cancelPF, remoteBase, _, err := startSyncthingPortForward(ctx, cc.opts, cc.namespace, pod)
	if err != nil {
		return nil, fmt.Errorf("port-forward to syncthing sidecar: %w", err)
	}
	live.remoteBase, live.close = remoteBase, cancelPF
	if err := waitSyncthingAPI(ctx, live.remoteBase, live.remoteKey, syncthingAPIReadyTimeout); err != nil {
		live.close()
		return nil, fmt.Errorf("remote syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(ctx, live.localBase, live.localKey, syncthingAPIReadyTimeout); err != nil {
		live.close()
		return nil, fmt.Errorf("local syncthing not ready: %w", err)
	}
	if live.localID, err = syncthingDeviceID(ctx, live.localBase, live.localKey); err != nil {
		live.close()
		return nil, fmt.Errorf("read local syncthing device id: %w", err)
	}
	if live.remoteID, err = syncthingDeviceID(ctx, live.remoteBase, live.remoteKey); err != nil {
		live.close()
		return nil, fmt.Errorf("read remote syncthing device id: %w", err)
	}
	return live, nil
}

// ensureFolder adds one folder to both daemons without disturbing the folders
// already syncing.
func (l *liveSync) ensureFolder(ctx context.Context, folder syncFolder, syncCfg syncthingSyncConfig) error {
	localType, remoteType := folderTypesForMode(folder.mode)
	if err := ensureSyncthingFolder(ctx, l.localBase, l.localKey, l.localID, l.remoteID, folder.id, folder.absLocal, localType, syncCfg); err != nil {
		return fmt.Errorf("configure local syncthing (%s): %w", folder.id, err)
	}
	if err := ensureSyncthingFolder(ctx, l.remoteBase, l.remoteKey, l.remoteID, l.localID, folder.id, folder.remote, remoteType, syncCfg); err != nil {
		return fmt.Errorf("configure remote syncthing (%s): %w", folder.id, err)
	}
	return nil
}

func (l *liveSync) removeFolder(ctx context.Context, folderID string) error {
	if err := removeSyncthingFolder(ctx, l.localBase, l.localKey, folderID); err != nil {
		return fmt.Errorf("remove local sync folder %s: %w", folderID, err)
	}
	if err := removeSyncthingFolder(ctx, l.remoteBase, l.remoteKey, folderID); err != nil {
		return fmt.Errorf("remove remote sync folder %s: %w", folderID, err)
	}
	return nil
}

// worktreeLiveTimeout bounds one in-place change to a running sync.
const worktreeLiveTimeout = time.Minute

// withLiveSync runs fn against the session's running sync daemons.
func withLiveSync(ctx context.Context, cc *commandContext, fn func(context.Context, *liveSync) error) error {
	ctx, cancel := context.WithTimeout(ctx, worktreeLiveTimeout)
	defer cancel()
	target, err := resolveTargetRef(ctx, cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
	if err != nil {
		return err
	}
	live, err := openLiveSync(ctx, cc, target.PodName)
	if err != nil {
		return err
	}
	defer live.close()
	return fn(ctx, live)
}

// advanceSyncConfigHash moves the recorded sync configuration from before to
// after an in-place folder change, so the next `okdev up` does not mistake
// the change for drift and restart sync. A record that does not match before
// is left alone: it marks drift of its own that `up` still has to act on.
func advanceSyncConfigHash(cc *commandContext, before, after []syncengine.Pair) error {
	changed, err := syncthingConfigChanged(cc.sessionName, syncthingSessionConfigHash(cc.cfg, before))
	if err != nil || changed {
		return err
	}
	return saveSyncthingConfigHash(cc.sessionName, syncthingSessionConfigHash(cc.cfg, after))
}

// startWorktreeSync (re)starts the session's background sync and waits until
// it has configured every folder. It must not be called with the worktree
// lock held: the sync start takes that lock itself.
func startWorktreeSync(ctx context.Context, cc *commandContext) error {
	if err := stopDetachedSyncthingSync(cc.sessionName); err != nil {
		return err
	}
	if err := stopLocalSyncthingForSession(cc.sessionName); err != nil {
		return err
	}
	// Whether this call started the process or another command got there
	// first, the bootstrap marker below is the one both write.
	if _, _, err := startDetachedSyncthingSync(cc.opts, "", cc.sessionName, cc.namespace, cc.cfgPath); err != nil {
		return err
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := waitForSyncthingBootstrapComplete(bootstrapCtx, cc.sessionName); err != nil {
		return fmt.Errorf("start sync for the worktree: %w", err)
	}
	return nil
}

// withoutWorktreePair returns pairs without the mapping of one worktree.
func withoutWorktreePair(pairs []syncengine.Pair, binding session.WorktreeBinding) []syncengine.Pair {
	out := make([]syncengine.Pair, 0, len(pairs))
	for _, pair := range pairs {
		if pair.Remote != binding.RemoteRoot {
			out = append(out, pair)
		}
	}
	return out
}
