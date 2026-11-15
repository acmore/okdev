package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
)

// errNotGitWorktree means the directory is simply not inside a Git working
// tree. Every other Git failure leaves the checkout unidentified, and callers
// must not read that as "not a worktree".
var errNotGitWorktree = errors.New("not inside a Git working tree")

// gitMemo remembers successful Git queries for the life of the process. One
// command resolves the same checkout several times (session selection, the
// worktree guards, the sync mappings), and nothing a single okdev invocation
// does changes the answers. Failures are not remembered.
var gitMemo = struct {
	sync.Mutex
	out map[string]string
}{out: map[string]string{}}

func gitOutput(dir string, args ...string) (string, error) {
	key := dir + "\x00" + strings.Join(args, "\x00")
	gitMemo.Lock()
	out, ok := gitMemo.out[key]
	gitMemo.Unlock()
	if ok {
		return out, nil
	}
	cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
	// The not-a-repository case is recognized by message, so pin the locale.
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	raw, err := cmd.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			msg := strings.TrimSpace(string(exitErr.Stderr))
			if strings.Contains(msg, "not a git repository") || strings.Contains(msg, "must be run in a work tree") {
				return "", errNotGitWorktree
			}
			if msg != "" {
				return "", fmt.Errorf("git %s: %s", args[0], msg)
			}
		}
		return "", fmt.Errorf("git %s: %w", args[0], err)
	}
	out = string(raw)
	gitMemo.Lock()
	gitMemo.out[key] = out
	gitMemo.Unlock()
	return out, nil
}

// gitWorktreeRoot returns the symlink-resolved top level of the working tree
// containing dir.
func gitWorktreeRoot(dir string) (string, error) {
	out, err := gitOutput(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(strings.TrimSpace(out))
}

// gitWorktreeRoots returns the symlink-resolved roots of every working tree of
// dir's repository that exists on disk, the main one included. One query
// answers "is this path a worktree of that repository" for any number of
// paths.
func gitWorktreeRoots(dir string) ([]string, error) {
	out, err := gitOutput(dir, "worktree", "list", "--porcelain", "-z")
	if err != nil {
		return nil, err
	}
	var roots []string
	for _, field := range strings.Split(out, "\x00") {
		root, ok := strings.CutPrefix(field, "worktree ")
		if !ok {
			continue
		}
		// Git keeps listing a worktree whose directory was deleted.
		if resolved, err := filepath.EvalSymlinks(root); err == nil {
			roots = append(roots, resolved)
		}
	}
	return roots, nil
}

// currentGitWorktree returns the root of the working tree the command runs in.
func currentGitWorktree() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	return gitWorktreeRoot(wd)
}

// sessionWorktreeBinding returns the attachment of the current checkout to
// the session, or nil when the command runs from the session's primary
// checkout, outside Git, or in an unrelated repository.
func sessionWorktreeBinding(sessionName string) (*session.WorktreeBinding, error) {
	if strings.TrimSpace(sessionName) == "" {
		return nil, nil
	}
	root, err := currentGitWorktree()
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
	siblings, err := gitWorktreeRoots(ownerRoot)
	if err != nil {
		return nil, err
	}
	if !slices.Contains(siblings, root) {
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
	var siblings []string
	if info.RepoRoot != "" {
		siblings, err = gitWorktreeRoots(info.RepoRoot)
		if err != nil {
			return nil, fmt.Errorf("resolve session repository for worktree sync: %w", err)
		}
	}
	states := make([]worktreeState, 0, len(bindings))
	for _, binding := range bindings {
		state := worktreeState{WorktreeBinding: binding}
		// Re-check what `worktree attach` verified: the recorded path must
		// still be a working tree of the session's repository. Syncing
		// anything else would upload a directory the user never attached.
		switch st, statErr := os.Stat(binding.LocalRoot); {
		case info.RepoRoot == "":
			state.Stale = "the session has no saved source root"
		case statErr != nil || !st.IsDir():
			state.Stale = "its local directory no longer exists"
		case !slices.Contains(siblings, binding.LocalRoot):
			state.Stale = "it is no longer a worktree of the session's Git repository"
		}
		states = append(states, state)
	}
	return states, nil
}

// worktreePair is the upload mapping of an attached worktree.
func worktreePair(binding session.WorktreeBinding) syncengine.Pair {
	return syncengine.Pair{Local: binding.LocalRoot, Remote: binding.RemoteRoot, Direction: config.SyncDirectionUp}
}

// worktreeSyncFolder is the sync folder of an attached worktree.
func worktreeSyncFolder(sessionName string, binding session.WorktreeBinding) syncFolder {
	pair := worktreePair(binding)
	return syncFolder{
		id:       sessionSyncFolderID(sessionName, 1, pair),
		absLocal: pair.Local,
		remote:   pair.Remote,
		mode:     pair.Direction,
		worktree: true,
	}
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
		pairs = append(pairs, worktreePair(state.WorktreeBinding))
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

// resolvedPath makes p absolute and resolves symlinks as far as the path
// exists, so that two spellings of one location compare equal.
func resolvedPath(p string) string {
	if resolved, err := canonicalPath(p); err == nil {
		return resolved
	}
	return filepath.Clean(p)
}

func syncPathsOverlap(a, b string) bool {
	a, b = resolvedPath(a), resolvedPath(b)
	return pathContainedIn(a, b) || pathContainedIn(b, a)
}

// syncPathContains reports whether child lies strictly below parent.
func syncPathContains(parent, child string) bool {
	parent, child = resolvedPath(parent), resolvedPath(child)
	return parent != child && pathContainedIn(parent, child)
}

// nestedSyncIgnorePatterns lists the subtrees of the primary sync root that
// must not travel through the primary folder: other sync mappings, and every
// linked Git worktree — attached or not, since a worktree is a second
// checkout, not part of the primary one.
func nestedSyncIgnorePatterns(pairs []syncengine.Pair) []string {
	if len(pairs) == 0 {
		return nil
	}
	primary := resolvedPath(pairs[0].Local)
	nested := make([]string, 0, len(pairs))
	for _, pair := range pairs[1:] {
		nested = append(nested, resolvedPath(pair.Local))
	}
	if roots, err := gitWorktreeRoots(primary); err == nil {
		nested = append(nested, roots...)
	}
	patternSet := make(map[string]bool)
	for _, child := range nested {
		if !syncPathContains(primary, child) {
			continue
		}
		if rel, err := filepath.Rel(primary, child); err == nil {
			patternSet["/"+filepath.ToSlash(rel)] = true
		}
	}
	patterns := make([]string, 0, len(patternSet))
	for pattern := range patternSet {
		patterns = append(patterns, pattern)
	}
	sort.Strings(patterns)
	return patterns
}
