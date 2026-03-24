package session

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/acmore/okdev/internal/workload"
)

func TestSaveLoadAndClearTarget(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	repoRootOnce = sync.Once{}
	repoRootVal = ""
	repoRootErr = nil

	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	repo := filepath.Join(tmp, "repo")
	if err := os.MkdirAll(filepath.Join(repo, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(repo); err != nil {
		t.Fatal(err)
	}
	defer os.Chdir(cwd)

	want := workload.TargetRef{PodName: "okdev-test", Container: "dev", Role: "primary"}
	if err := SaveTarget("test", want); err != nil {
		t.Fatalf("SaveTarget: %v", err)
	}
	got, err := LoadTarget("test")
	if err != nil {
		t.Fatalf("LoadTarget: %v", err)
	}
	if got != want {
		t.Fatalf("unexpected target: got %+v want %+v", got, want)
	}
	if err := ClearTarget("test"); err != nil {
		t.Fatalf("ClearTarget: %v", err)
	}
	got, err = LoadTarget("test")
	if err != nil {
		t.Fatalf("LoadTarget after clear: %v", err)
	}
	if got != (workload.TargetRef{}) {
		t.Fatalf("expected zero target after clear, got %+v", got)
	}
}
