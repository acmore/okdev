package session

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	got := sanitize("Repo/Feature_Branch")
	if got != "repo-feature-branch" {
		t.Fatalf("unexpected sanitize result %q", got)
	}
}

func TestResolveExplicit(t *testing.T) {
	name, err := Resolve("My_Session", "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "my-session" {
		t.Fatalf("unexpected resolved name %q", name)
	}
}

func TestSanitizeEmptyFallsBack(t *testing.T) {
	if got := sanitize("___"); got != "okdev-session" {
		t.Fatalf("unexpected sanitize fallback %q", got)
	}
}

func TestSanitizeTruncatesLongNames(t *testing.T) {
	got := sanitize(strings.Repeat("ab", 30))
	if len(got) > 50 {
		t.Fatalf("expected truncated name, got len=%d name=%q", len(got), got)
	}
}

func TestValidateNonEmptyAllowsSanitizedName(t *testing.T) {
	if err := ValidateNonEmpty("My Session"); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestResolveDefaultTemplateOmitsBranch(t *testing.T) {
	tmp := t.TempDir()
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	origWd, _ := os.Getwd()
	t.Cleanup(func() { _ = os.Chdir(origWd) })
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	resetRepoRootCache()
	t.Setenv("HOME", filepath.Join(tmp, "home"))
	t.Setenv("USER", "alice")

	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = repo
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v failed: %v\n%s", args, err, string(out))
		}
	}
	run("init", "-b", "feature/demo")

	name, err := Resolve("", "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "repo-alice" {
		t.Fatalf("unexpected resolved default name %q", name)
	}
}
