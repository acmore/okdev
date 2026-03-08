package session

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func resetRepoRootCache() {
	repoRootOnce = sync.Once{}
	repoRootVal = ""
	repoRootErr = nil
}

func TestActiveSessionPathUsesHomeDir(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	resetRepoRootCache()

	p, err := ActiveSessionPath()
	if err != nil {
		t.Fatalf("ActiveSessionPath error: %v", err)
	}
	if !strings.HasPrefix(p, filepath.Join(home, ".okdev", "sessions")+string(os.PathSeparator)) {
		t.Fatalf("expected path under ~/.okdev/sessions, got %q", p)
	}
	if strings.Contains(p, filepath.Join(repo, ".okdev")) {
		t.Fatalf("path should not be inside repo: %q", p)
	}
}

func TestLoadActiveSessionFallsBackToLegacyPath(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".okdev"), 0o755); err != nil {
		t.Fatal(err)
	}
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	resetRepoRootCache()

	if err := os.WriteFile(filepath.Join(repo, ".okdev", "active_session"), []byte("legacy-session\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := LoadActiveSession()
	if err != nil {
		t.Fatalf("LoadActiveSession error: %v", err)
	}
	if got != "legacy-session" {
		t.Fatalf("unexpected session: got %q want %q", got, "legacy-session")
	}
}
