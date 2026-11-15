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
