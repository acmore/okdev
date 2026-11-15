package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"time"

	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

// worktreeLockWait bounds how long attach and detach wait for another okdev
// operation on the same session (a sync start, or another attach).
const worktreeLockWait = 10 * time.Minute

func newWorktreeCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "worktree", Short: "Attach Git worktrees to one running session"}
	cmd.AddCommand(newWorktreeAttachCmd(opts), newWorktreeDetachCmd(opts), newWorktreeListCmd(opts))
	return cmd
}

// withWorktreeLock runs fn under the session's worktree lock, telling the
// user when it has to wait for another process.
func withWorktreeLock(ctx context.Context, errOut io.Writer, sessionName string, fn func() error) error {
	ctx, cancel := context.WithTimeout(ctx, worktreeLockWait)
	defer cancel()
	return session.WithWorktreeLock(ctx, sessionName, func() {
		fmt.Fprintf(errOut, "Waiting for another okdev operation on session %s to finish\n", sessionName)
	}, fn)
}

func newWorktreeAttachCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use: "attach", Short: "Sync this Git worktree into an existing session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Session == "" {
				return fmt.Errorf("worktree attach needs --session <existing-session>")
			}
			root, err := currentGitWorktree()
			if err != nil {
				return fmt.Errorf("run from a Git worktree: %w", err)
			}
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if cc.cfg.Spec.AttachOnly != nil {
				return fmt.Errorf("worktree attach requires a managed session")
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			info, err := session.LoadInfo(cc.sessionName)
			if err != nil {
				return err
			}
			if info.RepoRoot == "" {
				return fmt.Errorf("session %s has no saved source root; re-run `okdev up` from its original checkout", cc.sessionName)
			}
			ownerRoot, err := filepath.EvalSymlinks(info.RepoRoot)
			if err != nil {
				return err
			}
			if ownerRoot == root {
				return fmt.Errorf("%s already owns the primary sync mapping", root)
			}
			siblings, err := gitWorktreeRoots(ownerRoot)
			if err != nil {
				return err
			}
			if !slices.Contains(siblings, root) {
				return fmt.Errorf("worktree %s is not part of the session's Git repository", root)
			}
			pods, err := cc.kube.ListPods(cmd.Context(), cc.namespace, false, "okdev.io/managed=true,okdev.io/session="+cc.sessionName)
			if err != nil {
				return err
			}
			if len(pods) != 1 {
				return fmt.Errorf("worktree attach currently requires exactly one session pod (found %d)", len(pods))
			}
			binding := session.WorktreeBinding{ID: session.WorktreeID(root), LocalRoot: root}
			binding.RemoteRoot = session.WorktreeRemoteRoot(binding.ID)

			// Record the attachment first, so that whichever process ends up
			// configuring sync — this one in place, or a sync start — sees it.
			alreadyAttached := false
			err = withWorktreeLock(cmd.Context(), cmd.ErrOrStderr(), cc.sessionName, func() error {
				if health, _ := checkSyncHealth(cc.sessionName); health == syncHealthPaused {
					return fmt.Errorf("sync is paused; resume it before attaching a worktree")
				}
				bindings, err := session.LoadWorktreeBindings(cc.sessionName)
				if err != nil {
					return err
				}
				if slices.Contains(bindings, binding) {
					alreadyAttached = true
					return nil
				}
				pairs, err := sessionSyncPairs(cc)
				if err != nil {
					return err
				}
				if err := checkWorktreeOverlap(pairs, binding); err != nil {
					return err
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				// A sync start prepares the root too; doing it here first
				// rejects an unusable Pod before anything is recorded.
				if err := prepareWorktreeRemoteRoot(ctx, cc.kube, cc.namespace, pods[0].Name, resolveTargetContainer(cc.cfg), binding.RemoteRoot); err != nil {
					return err
				}
				// Ignore rules must be on disk before the folder exists, or
				// its first scan would upload the worktree's Git metadata.
				if err := writeLocalSTIgnore(root); err != nil {
					return err
				}
				if _, err := mergeManagedSTIgnoreBlock(root, []string{".git"}); err != nil {
					return fmt.Errorf("exclude worktree Git metadata: %w", err)
				}
				return session.SaveWorktreeBinding(cc.sessionName, binding)
			})
			if err != nil {
				return err
			}
			if err := syncAttachedWorktree(cmd.Context(), cmd.ErrOrStderr(), cc, binding); err != nil {
				if !alreadyAttached {
					_ = withWorktreeLock(cmd.Context(), io.Discard, cc.sessionName, func() error {
						if rmErr := session.RemoveWorktreeBinding(cc.sessionName, binding.ID); rmErr != nil {
							return rmErr
						}
						if health, _ := checkSyncHealth(cc.sessionName); health == syncHealthActive {
							_ = removeWorktreeFolderLive(cmd.Context(), cc, binding)
						}
						return nil
					})
				}
				return err
			}
			if err := session.SaveActiveSession(cc.sessionName); err != nil {
				return err
			}
			if alreadyAttached {
				fmt.Fprintf(cmd.OutOrStdout(), "Worktree already attached: %s -> %s\n", root, binding.RemoteRoot)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Attached %s -> %s (session %s)\n", root, binding.RemoteRoot, cc.sessionName)
			}
			return nil
		},
	}
}

// checkWorktreeOverlap rejects an attachment whose local or remote root would
// make two sync folders cover the same files. Being nested inside the primary
// root is the one allowed overlap: the primary folder excludes linked
// worktrees.
func checkWorktreeOverlap(pairs []syncengine.Pair, binding session.WorktreeBinding) error {
	for i, pair := range pairs {
		if syncPathsOverlap(pair.Remote, binding.RemoteRoot) {
			return fmt.Errorf("worktree remote root %s overlaps configured sync root %s", binding.RemoteRoot, pair.Remote)
		}
		if !syncPathsOverlap(pair.Local, binding.LocalRoot) {
			continue
		}
		if i > 0 {
			return fmt.Errorf("worktree %s overlaps another sync root %s", binding.LocalRoot, pair.Local)
		}
		if !syncPathContains(pair.Local, binding.LocalRoot) {
			return fmt.Errorf("worktree %s overlaps primary sync root %s", binding.LocalRoot, pair.Local)
		}
	}
	return nil
}

// syncAttachedWorktree makes the session's sync carry a recorded attachment.
// A healthy running sync gets the folder added in place, leaving every other
// folder syncing; only a stopped or broken sync is (re)started.
func syncAttachedWorktree(ctx context.Context, errOut io.Writer, cc *commandContext, binding session.WorktreeBinding) error {
	if health, _ := checkSyncHealth(cc.sessionName); health != syncHealthActive {
		if err := startWorktreeSync(ctx, cc); err != nil {
			return err
		}
	}
	// Also after a start: it normally configured the folder already and this
	// changes nothing, but a start that began before the attachment was
	// recorded did not know about it.
	return withWorktreeLock(ctx, errOut, cc.sessionName, func() error {
		pairs, states, err := sessionSyncPairsAndWorktrees(cc)
		if err != nil {
			return err
		}
		index := slices.IndexFunc(states, func(state worktreeState) bool { return state.ID == binding.ID })
		if index < 0 {
			return fmt.Errorf("worktree %s was detached while it was being attached", binding.LocalRoot)
		}
		if reason := states[index].Stale; reason != "" {
			return fmt.Errorf("worktree %s cannot be synced: %s", binding.LocalRoot, reason)
		}
		err = withLiveSync(ctx, cc, func(ctx context.Context, live *liveSync) error {
			return live.ensureFolder(ctx, worktreeSyncFolder(cc.sessionName, binding), syncthingSyncConfigFor(cc.cfg))
		})
		if err != nil {
			return fmt.Errorf("add worktree to the running sync: %w", err)
		}
		return advanceSyncConfigHash(cc, withoutWorktreePair(pairs, binding), pairs)
	})
}

func removeWorktreeFolderLive(ctx context.Context, cc *commandContext, binding session.WorktreeBinding) error {
	return withLiveSync(ctx, cc, func(ctx context.Context, live *liveSync) error {
		return live.removeFolder(ctx, worktreeSyncFolder(cc.sessionName, binding).id)
	})
}

func newWorktreeDetachCmd(opts *Options) *cobra.Command {
	var id string
	var deleteRemote bool
	cmd := &cobra.Command{
		Use: "detach", Short: "Stop syncing a worktree without stopping the session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if id == "" {
				root, err := currentGitWorktree()
				if err != nil {
					return fmt.Errorf("run from the attached worktree or pass --id: %w", err)
				}
				id = session.WorktreeID(root)
			}
			var detached session.WorktreeBinding
			syncRunning := false
			err = withWorktreeLock(cmd.Context(), cmd.ErrOrStderr(), cc.sessionName, func() error {
				health, _ := checkSyncHealth(cc.sessionName)
				if health == syncHealthPaused {
					return fmt.Errorf("sync is paused; resume it before detaching a worktree")
				}
				bindings, err := session.LoadWorktreeBindings(cc.sessionName)
				if err != nil {
					return err
				}
				index := slices.IndexFunc(bindings, func(binding session.WorktreeBinding) bool { return binding.ID == id })
				if index < 0 {
					return fmt.Errorf("worktree %s is not attached to session %s", id, cc.sessionName)
				}
				detached = bindings[index]
				before, err := sessionSyncPairs(cc)
				if err != nil {
					return err
				}
				if err := session.RemoveWorktreeBinding(cc.sessionName, id); err != nil {
					return err
				}
				// A sync that is not running has nothing to update: its next
				// start simply no longer includes the folder.
				if health != syncHealthActive {
					return nil
				}
				syncRunning = true
				if err := removeWorktreeFolderLive(cmd.Context(), cc, detached); err != nil {
					_ = session.SaveWorktreeBinding(cc.sessionName, detached)
					return fmt.Errorf("remove worktree from the running sync: %w", err)
				}
				return advanceSyncConfigHash(cc, before, withoutWorktreePair(before, detached))
			})
			if err != nil {
				return err
			}
			// The tree selected this session when it attached. Kept, the
			// selection would outlive the session and could only produce
			// "not attached" errors.
			if err := session.ClearActiveSessionFor(detached.LocalRoot, cc.sessionName); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to clear the session selection of %s: %v\n", detached.LocalRoot, err)
			}
			if !deleteRemote {
				fmt.Fprintf(cmd.OutOrStdout(), "Detached %s; remote files were kept in %s (remove them with `--delete-remote`)\n", id, detached.RemoteRoot)
				return nil
			}
			if !syncRunning {
				// The remote daemon may still hold the folder; deleting under
				// it is left to a moment when that can be checked.
				return fmt.Errorf("detached %s, but its remote files in %s were not deleted: sync is not running, so the Pod could not be updated first", id, detached.RemoteRoot)
			}
			if err := deleteWorktreeRemoteRoot(cmd.Context(), cc, detached); err != nil {
				return fmt.Errorf("detached %s, but its remote files in %s were not deleted: %w", id, detached.RemoteRoot, err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "Detached %s; remote files in %s were deleted\n", id, detached.RemoteRoot)
			return nil
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "Attached worktree ID (for removed worktrees)")
	cmd.Flags().BoolVar(&deleteRemote, "delete-remote", false, "Also delete the worktree's files in the Pod")
	return cmd
}

// deleteWorktreeRemoteRoot removes a detached worktree's directory from the
// Pod's runtime volume.
func deleteWorktreeRemoteRoot(ctx context.Context, cc *commandContext, binding session.WorktreeBinding) error {
	// Bindings are validated on load, so this is only a last line of defense
	// in front of an `rm -rf` run as the sidecar's user.
	if binding.RemoteRoot != session.WorktreeRemoteRoot(binding.ID) || !session.IsWorktreeRemoteRoot(binding.RemoteRoot) {
		return fmt.Errorf("refusing to delete %s: not a worktree root", binding.RemoteRoot)
	}
	ctx, cancel := context.WithTimeout(ctx, worktreeLiveTimeout)
	defer cancel()
	target, err := resolveTargetRef(ctx, cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
	if err != nil {
		return err
	}
	_, err = execInSyncthingContainer(ctx, cc.kube, cc.namespace, target.PodName, "rm -rf -- "+syncengine.ShellEscape(binding.RemoteRoot))
	return err
}

func newWorktreeListCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use: "list", Short: "List worktrees attached to the selected session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			states, err := sessionWorktreeStates(cc.sessionName)
			if err != nil {
				return err
			}
			if opts.Output == "json" {
				if states == nil {
					states = []worktreeState{}
				}
				return json.NewEncoder(cmd.OutOrStdout()).Encode(states)
			}
			for _, state := range states {
				line := fmt.Sprintf("%s  %s -> %s", state.ID, state.LocalRoot, state.RemoteRoot)
				if state.Stale != "" {
					line += "  (not syncing: " + state.Stale + ")"
				}
				fmt.Fprintln(cmd.OutOrStdout(), line)
			}
			return nil
		},
	}
}
