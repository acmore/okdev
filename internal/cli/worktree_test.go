package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
)

func TestCommandInWorkDirPreservesArguments(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "work tree")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	argv := commandInWorkDir([]string{"sh", "-c", `printf '%s\n%s\n' "$PWD" "$1"`, "arg0", "a;$(false)"}, dir)
	out, err := exec.Command(argv[0], argv[1:]...).CombinedOutput()
	if err != nil {
		t.Fatalf("run wrapped command: %v: %s", err, out)
	}
	if got, want := string(out), dir+"\na;$(false)\n"; got != want {
		t.Fatalf("output = %q, want %q", got, want)
	}
}

func TestNestedSyncIgnorePatternsIncludesWorktree(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, ".worktrees", "agent-a"), 0o755); err != nil {
		t.Fatal(err)
	}
	pairs := []syncengine.Pair{
		{Local: root, Remote: "/workspace"},
		{Local: filepath.Join(root, ".worktrees", "agent-a"), Remote: "/var/okdev/worktrees/a"},
		{Local: filepath.Join(t.TempDir(), "agent-b"), Remote: "/var/okdev/worktrees/b"},
	}
	patterns := nestedSyncIgnorePatterns(pairs)
	if len(patterns) != 1 || patterns[0] != "/.worktrees/agent-a" {
		t.Fatalf("patterns = %q", patterns)
	}
}

func TestNestedGitWorktreeIsExcludedBeforeAttach(t *testing.T) {
	main := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", main}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	run("init", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	run("add", "README")
	run("-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	run("worktree", "add", "--detach", filepath.Join(main, ".worktrees", "agent-a"))
	patterns := nestedSyncIgnorePatterns([]syncengine.Pair{{Local: main, Remote: "/workspace"}})
	if len(patterns) != 1 || patterns[0] != "/.worktrees/agent-a" {
		t.Fatalf("patterns = %q", patterns)
	}
}

func TestWorktreeDirectionCannotBeOverridden(t *testing.T) {
	pairs := []syncengine.Pair{
		{Local: "/source", Remote: "/workspace", Direction: "up"},
		{Local: "/linked", Remote: "/var/okdev/worktrees/1234567890abcdef", Direction: "up"},
	}
	if _, err := resolveSyncFolders("session", "down", pairs); err == nil || !strings.Contains(err.Error(), "must keep direction up") {
		t.Fatalf("down override error = %v", err)
	}
	if _, err := resolveSyncFolders("session", "up", pairs); err != nil {
		t.Fatalf("up override: %v", err)
	}
}

func TestSessionWorktreeBindingRejectsUnattachedSibling(t *testing.T) {
	main := t.TempDir()
	runGit := func(dir string, args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	runGit(main, "init", "-b", "main")
	if err := os.WriteFile(filepath.Join(main, "README"), []byte("test\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	runGit(main, "add", "README")
	runGit(main, "-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "-m", "initial")
	linked := filepath.Join(t.TempDir(), "linked")
	runGit(main, "worktree", "add", "--detach", linked)
	linked, err := filepath.EvalSymlinks(linked)
	if err != nil {
		t.Fatal(err)
	}
	name := "okdev-worktree-test-" + session.WorktreeID(main)
	dir, err := session.SessionDir(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	if err := session.SaveInfo(session.Info{Name: name, RepoRoot: main}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(linked)
	if _, err := sessionWorktreeBinding(name); err == nil || !strings.Contains(err.Error(), "not attached") {
		t.Fatalf("unattached worktree error = %v", err)
	}
	binding := session.WorktreeBinding{ID: session.WorktreeID(linked), LocalRoot: linked}
	binding.RemoteRoot = session.WorktreeRemoteRoot(binding.ID)
	if err := session.SaveWorktreeBinding(name, binding); err != nil {
		t.Fatal(err)
	}
	got, err := sessionWorktreeBinding(name)
	if err != nil || got == nil || got.RemoteRoot != binding.RemoteRoot {
		t.Fatalf("bound worktree = %+v, %v", got, err)
	}
}

func initTestRepo(t *testing.T) string {
	t.Helper()
	main, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"init", "-b", "main"},
		{"-c", "user.name=Test", "-c", "user.email=test@example.com", "commit", "--allow-empty", "-m", "initial"},
	} {
		cmd := exec.Command("git", append([]string{"-C", main}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v: %s", args, err, out)
		}
	}
	return main
}

func addTestWorktree(t *testing.T, main string) string {
	t.Helper()
	parent, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	linked := filepath.Join(parent, "linked")
	cmd := exec.Command("git", "-C", main, "worktree", "add", "--detach", linked)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git worktree add: %v: %s", err, out)
	}
	return linked
}

func saveTestBinding(t *testing.T, name, root string) session.WorktreeBinding {
	t.Helper()
	binding := session.WorktreeBinding{ID: session.WorktreeID(root), LocalRoot: root}
	binding.RemoteRoot = session.WorktreeRemoteRoot(binding.ID)
	if err := session.SaveWorktreeBinding(name, binding); err != nil {
		t.Fatal(err)
	}
	return binding
}

// A config below the Git top level must keep syncing its own project
// directory after `okdev up` records the repository root: the trust check
// approves relative roots against the config's project, so the engine has to
// resolve them the same way.
func TestSessionSyncPairsAnchorRelativeRootsAtConfigProject(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	repo := initTestRepo(t)
	project := filepath.Join(repo, "services", "api")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	cfg, cfgPath := trustTestConfig(t, project)
	cfg.Spec.Sync.Paths = []config.SyncPathSpec{{Local: ".", Remote: "/workspace"}}
	if err := session.SaveInfo(session.Info{Name: "anchor", RepoRoot: repo, ConfigPath: cfgPath}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(repo)
	pairs, err := sessionSyncPairs(&commandContext{cfg: cfg, cfgPath: cfgPath, sessionName: "anchor"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Local != project {
		t.Fatalf("sync root = %+v, want %s", pairs, project)
	}
	caps, err := localCapabilities(cfg, cfgPath)
	if err != nil || len(caps) != 0 {
		t.Fatalf("project-local sync should need no approval: %v, %v", caps, err)
	}
}

// When Git cannot identify the checkout, a session with attachments must not
// be treated as if the command ran from its primary checkout.
func TestSessionWorktreeBindingFailsClosedWhenGitFails(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := initTestRepo(t)
	linked := addTestWorktree(t, main)
	if err := session.SaveInfo(session.Info{Name: "closed", RepoRoot: main}); err != nil {
		t.Fatal(err)
	}
	t.Chdir(linked)
	t.Setenv("PATH", t.TempDir()) // no git

	if binding, err := sessionWorktreeBinding("closed"); err != nil || binding != nil {
		t.Fatalf("session without attachments = %+v, %v", binding, err)
	}
	saveTestBinding(t, "closed", linked)
	if _, err := sessionWorktreeBinding("closed"); err == nil || !strings.Contains(err.Error(), "could not be identified") {
		t.Fatalf("git failure with attachments = %v", err)
	}
	if err := rejectSharedSessionLifecycle("closed", "down"); err == nil {
		t.Fatal("lifecycle guard passed without identifying the checkout")
	}
}

func TestSessionWorktreeBindingOutsideGitIsUnbound(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := initTestRepo(t)
	linked := addTestWorktree(t, main)
	if err := session.SaveInfo(session.Info{Name: "outside", RepoRoot: main}); err != nil {
		t.Fatal(err)
	}
	saveTestBinding(t, "outside", linked)
	t.Chdir(t.TempDir())
	if binding, err := sessionWorktreeBinding("outside"); err != nil || binding != nil {
		t.Fatalf("outside Git = %+v, %v", binding, err)
	}
}

func TestStaleWorktreeIsReportedAndNotSynced(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := initTestRepo(t)
	live := addTestWorktree(t, main)
	gone := addTestWorktree(t, main)
	cfg, cfgPath := trustTestConfig(t, main)
	cfg.Spec.Sync.Paths = []config.SyncPathSpec{{Local: ".", Remote: "/workspace"}}
	if err := session.SaveInfo(session.Info{Name: "stale", RepoRoot: main, ConfigPath: cfgPath}); err != nil {
		t.Fatal(err)
	}
	saveTestBinding(t, "stale", live)
	goneBinding := saveTestBinding(t, "stale", gone)
	if err := os.RemoveAll(gone); err != nil {
		t.Fatal(err)
	}

	pairs, states, err := sessionSyncPairsAndWorktrees(&commandContext{cfg: cfg, cfgPath: cfgPath, sessionName: "stale"})
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 || pairs[1].Local != live {
		t.Fatalf("pairs = %+v", pairs)
	}
	if len(states) != 2 {
		t.Fatalf("states = %+v", states)
	}
	var warnings strings.Builder
	warnStaleWorktrees(&warnings, states)
	if got := warnings.String(); strings.Count(got, "\n") != 1 || !strings.Contains(got, gone) || !strings.Contains(got, "--id "+goneBinding.ID) {
		t.Fatalf("warnings = %q", got)
	}
}

// The runtime volume is recreated with the Pod, so the sync start — not only
// `worktree attach` — must open a worktree root to the dev container's user.
func TestWorktreeFoldersArePreparedForTheDevUser(t *testing.T) {
	folders, err := resolveSyncFolders("session", "", []syncengine.Pair{
		{Local: "/source", Remote: "/workspace", Direction: "bi"},
		{Local: "/linked", Remote: "/var/okdev/worktrees/1234567890abcdef", Direction: "up"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if folders[0].worktree || !folders[1].worktree {
		t.Fatalf("worktree flags = %+v", folders)
	}
	script := worktreeRemoteRootScript(folders[1].remote)
	if !strings.Contains(script, "mkdir -p") || !strings.Contains(script, "chmod 0777") {
		t.Fatalf("script = %q", script)
	}
}
