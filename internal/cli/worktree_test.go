package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
// `worktree attach` — prepares worktree roots, and it hands them to the dev
// container's user instead of opening them to everyone.
func TestWorktreeRootIsOwnedByTheDevUser(t *testing.T) {
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

	uid, gid, ok := parseContainerOwner("1000\n100\n")
	if !ok || uid != 1000 || gid != 100 {
		t.Fatalf("owner = %d:%d, %v", uid, gid, ok)
	}
	for _, bad := range []string{"", "1000", "root root", "1000 100 extra", "-1 0"} {
		if _, _, ok := parseContainerOwner(bad); ok {
			t.Fatalf("parsed owner from %q", bad)
		}
	}

	dir := filepath.Join(t.TempDir(), "work tree")
	run := func(script string) os.FileMode {
		t.Helper()
		if out, err := exec.Command("sh", "-c", script).CombinedOutput(); err != nil {
			t.Fatalf("%s: %v: %s", script, err, out)
		}
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		return info.Mode().Perm()
	}
	if mode := run(worktreeRemoteRootScript(dir, os.Getuid(), os.Getgid(), true)); mode != 0o755 {
		t.Fatalf("owned root mode = %o", mode)
	}
	// Ownership that cannot be handed over leaves the tree usable.
	if os.Getuid() != 0 {
		if mode := run(worktreeRemoteRootScript(dir, 0, 0, true)); mode != 0o777 {
			t.Fatalf("root mode after a refused chown = %o", mode)
		}
	}
	if mode := run(worktreeRemoteRootScript(dir, 0, 0, false)); mode != 0o777 {
		t.Fatalf("root mode for an unknown owner = %o", mode)
	}
}

// A mapping's folder ID is derived from the config's spelling of its local
// root, so resolving relative roots to absolute paths must not rename the
// folders of sessions created by earlier releases.
func TestRelativeMappingKeepsItsFolderID(t *testing.T) {
	project := t.TempDir()
	cfg, cfgPath := trustTestConfig(t, project)
	cfg.Spec.Sync.Paths = []config.SyncPathSpec{{Local: ".", Remote: "/workspace"}, {Local: "../sibling", Remote: "/sibling"}}
	pairs, err := configSyncPairs(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(pairs[1].Local) {
		t.Fatalf("local root not resolved: %+v", pairs[1])
	}
	want := sessionSyncFolderID("session", 1, syncengine.Pair{Local: "../sibling", Remote: "/sibling"})
	if got := sessionSyncFolderID("session", 1, pairs[1]); got != want {
		t.Fatalf("folder ID = %s, want %s", got, want)
	}
}

// One command asks Git about the same checkout several times; only the first
// question may cost a process.
func TestGitQueriesAreAnsweredOncePerProcess(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	main := initTestRepo(t)
	linked := addTestWorktree(t, main)
	if err := session.SaveInfo(session.Info{Name: "memo", RepoRoot: main}); err != nil {
		t.Fatal(err)
	}
	saveTestBinding(t, "memo", linked)
	t.Chdir(linked)
	if binding, err := sessionWorktreeBinding("memo"); err != nil || binding == nil {
		t.Fatalf("binding = %+v, %v", binding, err)
	}
	t.Setenv("PATH", t.TempDir()) // any further Git process would fail
	if binding, err := sessionWorktreeBinding("memo"); err != nil || binding == nil {
		t.Fatalf("second lookup = %+v, %v", binding, err)
	}
	states, err := sessionWorktreeStates("memo")
	if err != nil || len(states) != 1 || states[0].Stale != "" {
		t.Fatalf("states = %+v, %v", states, err)
	}
}

// fakeSyncthingConfig serves /rest/config from memory and counts writes.
type fakeSyncthingConfig struct {
	cfg  map[string]any
	puts int
}

func (f *fakeSyncthingConfig) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/rest/config" {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodPut {
		f.puts++
		f.cfg = map[string]any{}
		_ = json.NewDecoder(r.Body).Decode(&f.cfg)
		return
	}
	_ = json.NewEncoder(w).Encode(f.cfg)
}

func (f *fakeSyncthingConfig) folderIDs() []string {
	var ids []string
	folders, _ := f.cfg["folders"].([]any)
	for _, folder := range folders {
		ids = append(ids, asString(folder.(map[string]any)["id"]))
	}
	return ids
}

// Attaching and detaching change one folder of a running daemon in place;
// the peer device and the folders already syncing are not rewritten.
func TestWorktreeFolderIsAddedAndRemovedInPlace(t *testing.T) {
	fake := &fakeSyncthingConfig{cfg: map[string]any{
		"devices": []any{
			map[string]any{"deviceID": "SELF"},
			map[string]any{"deviceID": "PEER", "addresses": []any{"tcp://127.0.0.1:4242"}},
		},
		"folders": []any{map[string]any{"id": "okdev-s", "path": "/source", "type": "sendreceive"}},
	}}
	server := httptest.NewServer(fake)
	defer server.Close()
	ctx := context.Background()
	syncCfg := syncthingSyncConfig{rescanInterval: 60, watcherDelay: 1}

	ensure := func() {
		t.Helper()
		if err := ensureSyncthingFolder(ctx, server.URL, "key", "SELF", "PEER", "okdev-s-12345678", "/linked", "sendonly", syncCfg); err != nil {
			t.Fatal(err)
		}
	}
	ensure()
	if got := fake.folderIDs(); len(got) != 2 || got[0] != "okdev-s" || got[1] != "okdev-s-12345678" {
		t.Fatalf("folders = %v", got)
	}
	peer := fake.cfg["devices"].([]any)[1].(map[string]any)
	if addrs := peer["addresses"].([]any); len(addrs) != 1 || addrs[0] != "tcp://127.0.0.1:4242" {
		t.Fatalf("peer address rewritten: %v", addrs)
	}
	ensure()
	if fake.puts != 1 {
		t.Fatalf("an unchanged folder was written again: %d writes", fake.puts)
	}

	if err := ensureSyncthingFolder(ctx, server.URL, "key", "SELF", "STRANGER", "okdev-s-87654321", "/other", "sendonly", syncCfg); err == nil {
		t.Fatal("folder added for a peer the daemon does not know")
	}

	for range 2 {
		if err := removeSyncthingFolder(ctx, server.URL, "key", "okdev-s-12345678"); err != nil {
			t.Fatal(err)
		}
	}
	if got := fake.folderIDs(); len(got) != 1 || got[0] != "okdev-s" || fake.puts != 2 {
		t.Fatalf("folders after removal = %v, %d writes", got, fake.puts)
	}
}

func TestWorktreeOverlapRules(t *testing.T) {
	primary := t.TempDir()
	other := t.TempDir()
	pairs := []syncengine.Pair{{Local: primary, Remote: "/workspace"}, {Local: other, Remote: "/data"}}
	bind := func(root string) session.WorktreeBinding {
		binding := session.WorktreeBinding{ID: session.WorktreeID(root), LocalRoot: root}
		binding.RemoteRoot = session.WorktreeRemoteRoot(binding.ID)
		return binding
	}
	if err := checkWorktreeOverlap(pairs, bind(filepath.Join(primary, ".worktrees", "a"))); err != nil {
		t.Fatalf("nested worktree rejected: %v", err)
	}
	if err := checkWorktreeOverlap(pairs, bind(t.TempDir())); err != nil {
		t.Fatalf("sibling worktree rejected: %v", err)
	}
	for _, root := range []string{filepath.Dir(primary), filepath.Join(other, "sub"), other} {
		if err := checkWorktreeOverlap(pairs, bind(root)); err == nil {
			t.Fatalf("overlapping worktree %s accepted", root)
		}
	}
}
