package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

// errNotGitWorktree means the directory is simply not inside a Git working
// tree. Every other Git failure leaves the checkout unidentified, and callers
// must not read that as "not a worktree".
var errNotGitWorktree = errors.New("not inside a Git working tree")

func gitPath(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "rev-parse"}, args...)...)
	// The not-a-repository case is recognized by message, so pin the locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(msg, "not a git repository") || strings.Contains(msg, "must be run in a work tree") {
				return "", errNotGitWorktree
			}
			if msg != "" {
				return "", fmt.Errorf("git rev-parse: %s", msg)
			}
		}
		return "", fmt.Errorf("git rev-parse: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// gitWorktreeRoot returns the symlink-resolved top level of the working tree
// containing dir.
func gitWorktreeRoot(dir string) (string, error) {
	root, err := gitPath(dir, "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(root)
}

// gitCommonDir returns the symlink-resolved Git directory shared by every
// working tree of dir's repository; two checkouts are worktrees of one
// repository exactly when it matches.
func gitCommonDir(dir string) (string, error) {
	common, err := gitPath(dir, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(common)
}

func currentGitWorktree() (root, common string, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	root, err = gitWorktreeRoot(wd)
	if err != nil {
		return "", "", err
	}
	common, err = gitCommonDir(root)
	return root, common, err
}

// sessionWorktreeBinding returns the attachment of the current checkout to
// the session, or nil when the command runs from the session's primary
// checkout, outside Git, or in an unrelated repository.
func sessionWorktreeBinding(sessionName string) (*session.WorktreeBinding, error) {
	if strings.TrimSpace(sessionName) == "" {
		return nil, nil
	}
	root, common, err := currentGitWorktree()
	if errors.Is(err, errNotGitWorktree) {
		return nil, nil // explicit session calls outside Git remain supported
	}
	if err != nil {
		// The checkout could not be identified. With worktrees attached,
		// guessing "primary" would run this command against another tree's
		// code and let it through the lifecycle guards, so refuse. A session
		// without attachments has nothing to confuse it with.
		bindings, loadErr := session.LoadWorktreeBindings(sessionName)
		if loadErr != nil {
			return nil, loadErr
		}
		if len(bindings) == 0 {
			return nil, nil
		}
		return nil, fmt.Errorf("session %s has attached worktrees, but the current Git checkout could not be identified: %w", sessionName, err)
	}
	info, err := session.LoadInfo(sessionName)
	if err != nil {
		return nil, err
	}
	if info.RepoRoot == "" {
		return nil, nil
	}
	ownerRoot, err := filepath.EvalSymlinks(info.RepoRoot)
	if err != nil {
		return nil, err
	}
	if root == ownerRoot {
		return nil, nil
	}
	ownerCommon, err := gitCommonDir(ownerRoot)
	if err != nil {
		return nil, err
	}
	if common != ownerCommon {
		return nil, nil // unrelated repository using an explicit session
	}
	bindings, err := session.LoadWorktreeBindings(sessionName)
	if err != nil {
		return nil, err
	}
	for _, binding := range bindings {
		if binding.LocalRoot == root {
			return &binding, nil
		}
	}
	return nil, fmt.Errorf("worktree %s is not attached to session %s; run `okdev --session %s worktree attach`", root, sessionName, sessionName)
}

func rejectSharedSessionLifecycle(sessionName, action string) error {
	binding, err := sessionWorktreeBinding(sessionName)
	if err != nil {
		return err
	}
	if binding != nil {
		return fmt.Errorf("%s affects the shared session %s; run it from the primary checkout, or use `okdev worktree detach` to disconnect only %s", action, sessionName, binding.LocalRoot)
	}
	return nil
}

// worktreeState is an attachment plus the reason it cannot be synced right
// now, if any. A stale attachment stays recorded until it is detached, but
// contributes no sync folder.
type worktreeState struct {
	session.WorktreeBinding
	Stale string `json:"stale,omitempty"`
}

func sessionWorktreeStates(sessionName string) ([]worktreeState, error) {
	bindings, err := session.LoadWorktreeBindings(sessionName)
	if err != nil || len(bindings) == 0 {
		return nil, err
	}
	info, err := session.LoadInfo(sessionName)
	if err != nil {
		return nil, err
	}
	ownerCommon := ""
	if info.RepoRoot != "" {
		ownerCommon, err = gitCommonDir(info.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve session repository for worktree sync: %w", err)
		}
	}
	states := make([]worktreeState, 0, len(bindings))
	for _, binding := range bindings {
		state := worktreeState{WorktreeBinding: binding}
		if ownerCommon == "" {
			state.Stale = "the session has no saved source root"
		} else {
			state.Stale = worktreeStaleReason(binding, ownerCommon)
		}
		states = append(states, state)
	}
	return states, nil
}

// worktreeStaleReason re-checks what `worktree attach` verified: the recorded
// path must still be the root of a working tree of the session's repository.
// Syncing anything else would upload a directory the user never attached.
func worktreeStaleReason(binding session.WorktreeBinding, ownerCommon string) string {
	if st, err := os.Stat(binding.LocalRoot); err != nil || !st.IsDir() {
		return "its local directory no longer exists"
	}
	root, err := gitWorktreeRoot(binding.LocalRoot)
	if err != nil {
		return fmt.Sprintf("it could not be verified as a Git worktree (%v)", err)
	}
	if root != binding.LocalRoot {
		return "it is no longer the root of a Git worktree"
	}
	common, err := gitCommonDir(root)
	if err != nil {
		return fmt.Sprintf("it could not be verified as a Git worktree (%v)", err)
	}
	if common != ownerCommon {
		return "it no longer belongs to the session's Git repository"
	}
	return ""
}

// sessionSyncPairs returns the config's sync mappings followed by one upload
// mapping per attached worktree that can be synced.
func sessionSyncPairs(cc *commandContext) ([]syncengine.Pair, error) {
	pairs, _, err := sessionSyncPairsAndWorktrees(cc)
	return pairs, err
}

func sessionSyncPairsAndWorktrees(cc *commandContext) ([]syncengine.Pair, []worktreeState, error) {
	pairs, err := configSyncPairs(cc.cfg, cc.cfgPath)
	if err != nil {
		return nil, nil, err
	}
	states, err := sessionWorktreeStates(cc.sessionName)
	if err != nil {
		return nil, nil, err
	}
	for _, state := range states {
		if state.Stale != "" {
			continue
		}
		pairs = append(pairs, syncengine.Pair{Local: state.LocalRoot, Remote: state.RemoteRoot, Direction: config.SyncDirectionUp})
	}
	return pairs, states, nil
}

// warnStaleWorktrees reports attachments that are recorded but not syncing.
func warnStaleWorktrees(w io.Writer, states []worktreeState) {
	for _, state := range states {
		if state.Stale != "" {
			fmt.Fprintf(w, "warning: attached worktree %s is not syncing: %s. Run `okdev worktree detach --id %s` to remove it.\n", state.LocalRoot, state.Stale, state.ID)
		}
	}
}

func nestedSyncIgnorePatterns(pairs []syncengine.Pair) []string {
	if len(pairs) == 0 {
		return nil
	}
	primary, err := filepath.Abs(pairs[0].Local)
	if err != nil {
		return nil
	}
	if resolved, err := filepath.EvalSymlinks(primary); err == nil {
		primary = resolved
	}
	patternSet := make(map[string]bool)
	for _, pair := range pairs[1:] {
		child, err := filepath.Abs(pair.Local)
		if err != nil {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(child); err == nil {
			child = resolved
		}
		rel, err := filepath.Rel(primary, child)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			patternSet["/"+filepath.ToSlash(rel)] = true
		}
	}
	for _, pattern := range nestedGitWorktreeIgnorePatterns(primary) {
		patternSet[pattern] = true
	}
	patterns := make([]string, 0, len(patternSet))
	for pattern := range patternSet {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return patterns
}

func nestedGitWorktreeIgnorePatterns(primary string) []string {
	cmd := exec.Command("git", "-C", primary, "worktree", "list", "--porcelain", "-z")
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	var patterns []string
	for _, field := range strings.Split(string(out), "\x00") {
		if !strings.HasPrefix(field, "worktree ") {
			continue
		}
		root := strings.TrimPrefix(field, "worktree ")
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			continue
		}
		rel, err := filepath.Rel(primary, root)
		if err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			patterns = append(patterns, "/"+filepath.ToSlash(rel))
		}
	}
	return patterns
}

func syncPathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if resolved, err := filepath.EvalSymlinks(a); err == nil {
		a = resolved
	}
	if resolved, err := filepath.EvalSymlinks(b); err == nil {
		b = resolved
	}
	if a == b {
		return true
	}
	for _, pair := range [][2]string{{a, b}, {b, a}} {
		rel, err := filepath.Rel(pair[0], pair[1])
		if err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			return true
		}
	}
	return false
}

func syncPathContains(parent, child string) bool {
	if resolved, err := filepath.EvalSymlinks(parent); err == nil {
		parent = resolved
	}
	if resolved, err := filepath.EvalSymlinks(child); err == nil {
		child = resolved
	}
	rel, err := filepath.Rel(filepath.Clean(parent), filepath.Clean(child))
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func newWorktreeCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{Use: "worktree", Short: "Attach Git worktrees to one running session"}
	cmd.AddCommand(newWorktreeAttachCmd(opts), newWorktreeDetachCmd(opts), newWorktreeListCmd(opts))
	return cmd
}

func newWorktreeAttachCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use: "attach", Short: "Sync this Git worktree into an existing session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if opts.Session == "" {
				return fmt.Errorf("worktree attach needs --session <existing-session>")
			}
			root, common, err := currentGitWorktree()
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
			ownerCommon, err := gitCommonDir(ownerRoot)
			if err != nil {
				return err
			}
			if common != ownerCommon {
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
			return session.WithWorktreeLock(cc.sessionName, func() error {
				if health, _ := checkSyncHealth(cc.sessionName); health == syncHealthPaused {
					return fmt.Errorf("sync is paused; resume it before attaching a worktree")
				}
				bindings, err := session.LoadWorktreeBindings(cc.sessionName)
				if err != nil {
					return err
				}
				for _, existing := range bindings {
					if existing.ID == binding.ID && existing.LocalRoot == binding.LocalRoot {
						fmt.Fprintf(cmd.OutOrStdout(), "Worktree already attached: %s -> %s\n", root, existing.RemoteRoot)
						return session.SaveActiveSession(cc.sessionName)
					}
				}
				pairs, err := sessionSyncPairs(cc)
				if err != nil {
					return err
				}
				for i, pair := range pairs {
					if syncPathsOverlap(pair.Remote, binding.RemoteRoot) {
						return fmt.Errorf("worktree remote root %s overlaps configured sync root %s", binding.RemoteRoot, pair.Remote)
					}
					if i == 0 && syncPathsOverlap(pair.Local, binding.LocalRoot) && !syncPathContains(pair.Local, binding.LocalRoot) {
						return fmt.Errorf("worktree %s overlaps primary sync root %s", binding.LocalRoot, pair.Local)
					}
					if i > 0 && syncPathsOverlap(pair.Local, binding.LocalRoot) {
						return fmt.Errorf("worktree %s overlaps another sync root %s", binding.LocalRoot, pair.Local)
					}
				}
				ctx, cancel := context.WithTimeout(cmd.Context(), 30*time.Second)
				defer cancel()
				// The sync start below prepares the root again; doing it here
				// first rejects an unusable Pod before anything is recorded.
				if _, err := execInSyncthingContainer(ctx, cc.kube, cc.namespace, pods[0].Name, worktreeRemoteRootScript(binding.RemoteRoot)); err != nil {
					return fmt.Errorf("prepare remote worktree root: %w", err)
				}
				if err := verifyRemoteRootWritable(ctx, cc.kube, cc.namespace, pods[0].Name, resolveTargetContainer(cc.cfg), binding.RemoteRoot); err != nil {
					return err
				}
				if err := writeLocalSTIgnore(root); err != nil {
					return err
				}
				if _, err := mergeManagedSTIgnoreBlock(root, []string{".git"}); err != nil {
					return fmt.Errorf("exclude worktree Git metadata: %w", err)
				}
				if err := session.SaveWorktreeBinding(cc.sessionName, binding); err != nil {
					return err
				}
				if err := restartWorktreeSync(cmd.Context(), cc); err != nil {
					_ = session.RemoveWorktreeBinding(cc.sessionName, binding.ID)
					_ = restartWorktreeSync(cmd.Context(), cc)
					return err
				}
				if err := session.SaveActiveSession(cc.sessionName); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Attached %s -> %s (session %s)\n", root, binding.RemoteRoot, cc.sessionName)
				return nil
			})
		},
	}
}

func restartWorktreeSync(ctx context.Context, cc *commandContext) error {
	if health, _ := checkSyncHealth(cc.sessionName); health == syncHealthPaused {
		return fmt.Errorf("sync is paused; resume it before changing worktree attachments")
	}
	if err := stopDetachedSyncthingSync(cc.sessionName); err != nil {
		return err
	}
	if err := stopLocalSyncthingForSession(cc.sessionName); err != nil {
		return err
	}
	if _, _, err := startDetachedSyncthingSync(cc.opts, "", cc.sessionName, cc.namespace, cc.cfgPath); err != nil {
		return err
	}
	bootstrapCtx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	if err := waitForSyncthingBootstrapComplete(bootstrapCtx, cc.sessionName); err != nil {
		return fmt.Errorf("configure worktree sync: %w", err)
	}
	return nil
}

func newWorktreeDetachCmd(opts *Options) *cobra.Command {
	var id string
	cmd := &cobra.Command{
		Use: "detach", Short: "Stop syncing a worktree without stopping the session", Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if id == "" {
				root, _, err := currentGitWorktree()
				if err != nil {
					return fmt.Errorf("run from the attached worktree or pass --id: %w", err)
				}
				id = session.WorktreeID(root)
			}
			return session.WithWorktreeLock(cc.sessionName, func() error {
				if health, _ := checkSyncHealth(cc.sessionName); health == syncHealthPaused {
					return fmt.Errorf("sync is paused; resume it before detaching a worktree")
				}
				bindings, err := session.LoadWorktreeBindings(cc.sessionName)
				if err != nil {
					return err
				}
				var detached *session.WorktreeBinding
				for _, binding := range bindings {
					if binding.ID == id {
						detached = &binding
						break
					}
				}
				if detached == nil {
					return fmt.Errorf("worktree %s is not attached to session %s", id, cc.sessionName)
				}
				if err := session.RemoveWorktreeBinding(cc.sessionName, id); err != nil {
					return err
				}
				if err := restartWorktreeSync(cmd.Context(), cc); err != nil {
					_ = session.SaveWorktreeBinding(cc.sessionName, *detached)
					_ = restartWorktreeSync(cmd.Context(), cc)
					return err
				}
				// The tree selected this session when it attached. Kept, the
				// selection would outlive the session and could only produce
				// "not attached" errors.
				if err := session.ClearActiveSessionFor(detached.LocalRoot, cc.sessionName); err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to clear the session selection of %s: %v\n", detached.LocalRoot, err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Detached %s; remote files were kept\n", id)
				return nil
			})
		},
	}
	cmd.Flags().StringVar(&id, "id", "", "Attached worktree ID (for removed worktrees)")
	return cmd
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
