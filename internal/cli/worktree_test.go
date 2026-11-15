package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

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
