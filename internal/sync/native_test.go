package sync

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParsePairsDefault(t *testing.T) {
	pairs, err := ParsePairs(nil, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Local != "." || pairs[0].Remote != "/workspace" {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}
}

func TestParsePairsConfigured(t *testing.T) {
	pairs, err := ParsePairs([]string{".:/workspace", "./cfg:/etc/app"}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("unexpected pair count: %d", len(pairs))
	}
}

func TestParsePairsInvalid(t *testing.T) {
	if _, err := ParsePairs([]string{"invalid"}, "/workspace"); err == nil {
		t.Fatal("expected error")
	}
}

func TestShellEscape(t *testing.T) {
	got := ShellEscape("a'b")
	if got != "'a'\\''b'" {
		t.Fatalf("unexpected escaped shell string %q", got)
	}
}

func TestBuildRemoteTarCommand(t *testing.T) {
	cmd := buildRemoteTarCommand("/tmp/a.tar", "/workspace/app", []string{"*.pyc", "dir with space"})
	want := "'tar' '-cf' '/tmp/a.tar' '--exclude' '*.pyc' '--exclude' 'dir with space' '-C' '/workspace/app' '.'"
	if cmd != want {
		t.Fatalf("unexpected command:\nwant: %s\ngot:  %s", want, cmd)
	}
}

type blockingSyncClient struct{}

func (blockingSyncClient) CopyToPod(ctx context.Context, namespace, local, pod, remote string) error {
	<-ctx.Done()
	return ctx.Err()
}

func (blockingSyncClient) CopyFromPod(ctx context.Context, namespace, pod, remote, local string) error {
	return nil
}

func (blockingSyncClient) ExecSh(ctx context.Context, namespace, pod, script string) ([]byte, error) {
	return nil, nil
}

func TestRunOnceHonorsParentContextCancellation(t *testing.T) {
	tmp := t.TempDir()
	local := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(local, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(10 * time.Millisecond)
		cancel()
	}()

	err := RunOnce(ctx, "up", blockingSyncClient{}, "ns", "pod", []Pair{{Local: local, Remote: "/workspace/file.txt"}}, nil)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context cancellation error, got %v", err)
	}
}
