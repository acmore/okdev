package cli

import (
	"context"
	"encoding/json"
	"fmt"
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

func gitPath(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", dir, "rev-parse"}, args...)...)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func currentGitWorktree() (root, common string, err error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	root, err = gitPath(wd, "--show-toplevel")
	if err != nil {
		return "", "", err
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", "", err
	}
	common, err = gitPath(root, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", "", err
	}
	common, err = filepath.EvalSymlinks(common)
	return root, common, err
}

func sessionWorktreeBinding(sessionName string) (*session.WorktreeBinding, error) {
	root, common, err := currentGitWorktree()
	if err != nil {
		return nil, nil // explicit session calls outside Git remain supported
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
	ownerCommon, err := gitPath(ownerRoot, "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return nil, err
	}
	ownerCommon, err = filepath.EvalSymlinks(ownerCommon)
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

func sessionSyncPairs(cc *commandContext) ([]syncengine.Pair, error) {
	pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
	if err != nil {
		return nil, err
	}
	info, err := session.LoadInfo(cc.sessionName)
	if err != nil {
		return nil, err
	}
	base := info.RepoRoot
	if base == "" {
		base = config.RootDir(cc.cfgPath)
	}
	for i := range pairs {
		if !filepath.IsAbs(pairs[i].Local) {
			pairs[i].Local = filepath.Join(base, pairs[i].Local)
		}
	}
	bindings, err := session.LoadWorktreeBindings(cc.sessionName)
	if err != nil {
		return nil, err
	}
	ownerCommon := ""
	if len(bindings) > 0 && info.RepoRoot != "" {
		ownerCommon, err = gitPath(info.RepoRoot, "--path-format=absolute", "--git-common-dir")
		if err != nil {
			return nil, fmt.Errorf("resolve session repository for worktree sync: %w", err)
		}
		ownerCommon, err = filepath.EvalSymlinks(ownerCommon)
		if err != nil {
			return nil, err
		}
	}
	for _, binding := range bindings {
		if st, err := os.Stat(binding.LocalRoot); err != nil || !st.IsDir() {
			continue // removed worktrees stay listed but cannot recreate their paths
		}
		root, err := gitPath(binding.LocalRoot, "--show-toplevel")
		if err != nil {
			continue
		}
		root, err = filepath.EvalSymlinks(root)
		if err != nil || root != binding.LocalRoot {
			continue
		}
		common, err := gitPath(root, "--path-format=absolute", "--git-common-dir")
		if err != nil {
			continue
		}
		common, err = filepath.EvalSymlinks(common)
		if err != nil || common != ownerCommon {
			continue
		}
		pairs = append(pairs, syncengine.Pair{Local: binding.LocalRoot, Remote: binding.RemoteRoot, Direction: config.SyncDirectionUp})
	}
	return pairs, nil
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
			ownerCommon, err := gitPath(ownerRoot, "--path-format=absolute", "--git-common-dir")
			if err != nil {
				return err
			}
			ownerCommon, err = filepath.EvalSymlinks(ownerCommon)
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
				if _, err := execInSyncthingContainer(ctx, cc.kube, cc.namespace, pods[0].Name, fmt.Sprintf("mkdir -p %s && chmod 0777 %s", syncengine.ShellEscape(binding.RemoteRoot), syncengine.ShellEscape(binding.RemoteRoot))); err != nil {
					return fmt.Errorf("prepare remote worktree root: %w", err)
				}
				targetContainer := resolveTargetContainer(cc.cfg)
				probe := filepath.ToSlash(filepath.Join(binding.RemoteRoot, ".okdev-write-probe"))
				if _, err := cc.kube.ExecShInContainer(ctx, cc.namespace, pods[0].Name, targetContainer, fmt.Sprintf("touch %s && rm -f %s", syncengine.ShellEscape(probe), syncengine.ShellEscape(probe))); err != nil {
					return fmt.Errorf("remote worktree root %s is not writable in container %s: %w", binding.RemoteRoot, targetContainer, err)
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
				found := false
				for _, binding := range bindings {
					if binding.ID == id {
						found = true
						break
					}
				}
				if !found {
					return fmt.Errorf("worktree %s is not attached to session %s", id, cc.sessionName)
				}
				if err := session.RemoveWorktreeBinding(cc.sessionName, id); err != nil {
					return err
				}
				if err := restartWorktreeSync(cmd.Context(), cc); err != nil {
					for _, binding := range bindings {
						if binding.ID == id {
							_ = session.SaveWorktreeBinding(cc.sessionName, binding)
							break
						}
					}
					_ = restartWorktreeSync(cmd.Context(), cc)
					return err
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
			bindings, err := session.LoadWorktreeBindings(cc.sessionName)
			if err != nil {
				return err
			}
			if opts.Output == "json" {
				return json.NewEncoder(cmd.OutOrStdout()).Encode(bindings)
			}
			for _, binding := range bindings {
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %s -> %s\n", binding.ID, binding.LocalRoot, binding.RemoteRoot)
			}
			return nil
		},
	}
}
