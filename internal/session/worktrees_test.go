package session

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWorktreeBindingsRoundTrip(t *testing.T) {
	root := t.TempDir()
	name := "okdev-binding-test-" + WorktreeID(root)
	dir, err := SessionDir(name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	binding := WorktreeBinding{ID: WorktreeID(root), LocalRoot: root}
	binding.RemoteRoot = WorktreeRemoteRoot(binding.ID)
	if err := SaveWorktreeBinding(name, binding); err != nil {
		t.Fatal(err)
	}
	got, err := LoadWorktreeBindings(name)
	if err != nil || len(got) != 1 || got[0] != binding {
		t.Fatalf("bindings = %+v, %v", got, err)
	}
	path := filepath.Join(dir, "worktrees", binding.ID+".json")
	if info, err := os.Stat(path); err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("binding file mode = %v, %v", info, err)
	}
	if err := RemoveWorktreeBinding(name, binding.ID); err != nil {
		t.Fatal(err)
	}
	got, err = LoadWorktreeBindings(name)
	if err != nil || len(got) != 0 {
		t.Fatalf("bindings after removal = %+v, %v", got, err)
	}
}

func TestTeardownReleasesAttachedWorktrees(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	root := t.TempDir()
	other := t.TempDir()
	binding := WorktreeBinding{ID: WorktreeID(root), LocalRoot: root}
	binding.RemoteRoot = WorktreeRemoteRoot(binding.ID)
	if err := SaveWorktreeBinding("shared", binding); err != nil {
		t.Fatal(err)
	}
	stateRoot, err := stateRootDir()
	if err != nil {
		t.Fatal(err)
	}
	activePath := func(repoRoot string) string {
		return filepath.Join(stateRoot, workspacesDirName, repoStateKey(repoRoot), sessionFileName)
	}
	for repoRoot, name := range map[string]string{root: "shared", other: "elsewhere"} {
		if err := os.MkdirAll(filepath.Dir(activePath(repoRoot)), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(activePath(repoRoot), []byte(name+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	for _, repoRoot := range []string{root, other, t.TempDir()} {
		if err := ClearActiveSessionFor(repoRoot, "shared"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(activePath(root)); !os.IsNotExist(err) {
		t.Fatalf("attached worktree still selects the session: %v", err)
	}
	if _, err := os.Stat(activePath(other)); err != nil {
		t.Fatalf("a checkout on another session was cleared: %v", err)
	}

	// A binding file that no longer decodes must not survive the teardown.
	dir, err := SessionDir("shared")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, worktreeStateDir, "broken.json"), []byte("{"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ClearWorktreeBindings("shared"); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadWorktreeBindings("shared"); err != nil || len(got) != 0 {
		t.Fatalf("bindings after teardown = %+v, %v", got, err)
	}
}
