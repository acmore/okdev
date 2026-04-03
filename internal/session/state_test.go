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

func chdirRepo(t *testing.T, home, repo string) {
	t.Helper()
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	resetRepoRootCache()
}

func TestActiveSessionPathUsesWorkspaceDir(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	chdirRepo(t, home, repo)

	p, err := ActiveSessionPath()
	if err != nil {
		t.Fatalf("ActiveSessionPath error: %v", err)
	}
	if !strings.HasPrefix(p, filepath.Join(home, ".okdev", "workspaces")+string(os.PathSeparator)) {
		t.Fatalf("expected path under ~/.okdev/workspaces, got %q", p)
	}
}

func TestSessionDirUsesGlobalSessionsRoot(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	dir, err := SessionDir("My Session")
	if err != nil {
		t.Fatalf("SessionDir error: %v", err)
	}
	wantPrefix := filepath.Join(tmp, ".okdev", "sessions") + string(os.PathSeparator)
	if !strings.HasPrefix(dir, wantPrefix) {
		t.Fatalf("expected session dir under %q, got %q", wantPrefix, dir)
	}
	if filepath.Base(dir) != "my-session" {
		t.Fatalf("unexpected sanitized session dir %q", dir)
	}
}

func TestShutdownRequestLifecycle(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	chdirRepo(t, home, repo)

	requested, err := ShutdownRequested("test-session")
	if err != nil {
		t.Fatalf("ShutdownRequested before request error: %v", err)
	}
	if requested {
		t.Fatal("expected no shutdown request initially")
	}

	if err := RequestShutdown("test-session"); err != nil {
		t.Fatalf("RequestShutdown error: %v", err)
	}

	p, err := ShutdownRequestPath("test-session")
	if err != nil {
		t.Fatalf("ShutdownRequestPath error: %v", err)
	}
	if !strings.Contains(p, filepath.Join(".okdev", "sessions", "test-session", shutdownMarkerName)) {
		t.Fatalf("unexpected shutdown marker path %q", p)
	}

	requested, err = ShutdownRequested("test-session")
	if err != nil {
		t.Fatalf("ShutdownRequested after request error: %v", err)
	}
	if !requested {
		t.Fatal("expected shutdown request to exist")
	}

	if err := ClearShutdownRequest("test-session"); err != nil {
		t.Fatalf("ClearShutdownRequest error: %v", err)
	}

	requested, err = ShutdownRequested("test-session")
	if err != nil {
		t.Fatalf("ShutdownRequested after clear error: %v", err)
	}
	if requested {
		t.Fatal("expected shutdown request to be cleared")
	}
}

func TestRepoStateKeyIncludesSanitizedBaseAndHash(t *testing.T) {
	got := repoStateKey("/tmp/My Repo")
	if !strings.HasPrefix(got, "my-repo-") {
		t.Fatalf("unexpected repo state key prefix %q", got)
	}
	if len(got) <= len("my-repo-") {
		t.Fatalf("expected hashed suffix in repo state key %q", got)
	}
}

func TestSaveLoadActiveSession(t *testing.T) {
	tmp := t.TempDir()
	home := filepath.Join(tmp, "home")
	repo := filepath.Join(tmp, "repo")
	chdirRepo(t, home, repo)

	if err := SaveActiveSession("new-session"); err != nil {
		t.Fatalf("SaveActiveSession error: %v", err)
	}
	got, err := LoadActiveSession()
	if err != nil {
		t.Fatalf("LoadActiveSession error: %v", err)
	}
	if got != "new-session" {
		t.Fatalf("unexpected active session %q", got)
	}
}

func TestSaveLoadSessionInfo(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	if err := SaveInfo(Info{
		Name:         "sess-a",
		RepoRoot:     "/tmp/repo",
		ConfigPath:   "/tmp/repo/.okdev.yaml",
		Namespace:    "default",
		KubeContext:  "dev",
		Owner:        "alice",
		WorkloadType: "deployment",
	}); err != nil {
		t.Fatalf("SaveInfo error: %v", err)
	}

	info, err := LoadInfo("sess-a")
	if err != nil {
		t.Fatalf("LoadInfo error: %v", err)
	}
	if info.Name != "sess-a" || info.ConfigPath != "/tmp/repo/.okdev.yaml" || info.WorkloadType != "deployment" {
		t.Fatalf("unexpected session info: %+v", info)
	}
	if info.CreatedAt.IsZero() || info.LastUsedAt.IsZero() {
		t.Fatalf("expected timestamps to be set: %+v", info)
	}
}
