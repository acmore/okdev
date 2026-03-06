package sync

import (
	"context"
	"errors"
	"io"
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
	cmd := buildRemoteTarStreamCommand("/workspace/app", []string{"*.pyc", "dir with space"})
	want := "'tar' '-cf' '-' '--exclude' '*.pyc' '--exclude' 'dir with space' '-C' '/workspace/app' '.'"
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
func (blockingSyncClient) ExtractTarToPod(ctx context.Context, namespace, pod, remoteDir string, tarStream io.Reader) error {
	<-ctx.Done()
	return ctx.Err()
}
func (blockingSyncClient) StreamFromPod(ctx context.Context, namespace, pod, script string, w io.Writer) error {
	<-ctx.Done()
	return ctx.Err()
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

type noopSyncClient struct{}

func (noopSyncClient) CopyToPod(context.Context, string, string, string, string) error { return nil }
func (noopSyncClient) CopyFromPod(context.Context, string, string, string, string) error {
	return nil
}
func (noopSyncClient) ExtractTarToPod(context.Context, string, string, string, io.Reader) error {
	return nil
}
func (noopSyncClient) StreamFromPod(context.Context, string, string, string, io.Writer) error {
	return nil
}
func (noopSyncClient) ExecSh(context.Context, string, string, string) ([]byte, error) {
	return nil, nil
}

func TestRunOnceWithReportUpFile(t *testing.T) {
	ResetUploadFingerprintCache()
	tmp := t.TempDir()
	local := filepath.Join(tmp, "file.txt")
	content := []byte("hello")
	if err := os.WriteFile(local, content, 0o644); err != nil {
		t.Fatal(err)
	}

	stats, err := RunOnceWithReport(context.Background(), "up", noopSyncClient{}, "ns", "pod", []Pair{{Local: local, Remote: "/workspace/file.txt"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Paths != 1 {
		t.Fatalf("unexpected paths count: %d", stats.Paths)
	}
	if stats.UploadBytes != int64(len(content)) {
		t.Fatalf("unexpected upload bytes: %d", stats.UploadBytes)
	}

	stats2, err := RunOnceWithReport(context.Background(), "up", noopSyncClient{}, "ns", "pod", []Pair{{Local: local, Remote: "/workspace/file.txt"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if stats2.SkippedPaths != 1 {
		t.Fatalf("expected skipped path on unchanged file, got %d", stats2.SkippedPaths)
	}
}

func TestRunOnceWithOptionsForceBypassesFingerprint(t *testing.T) {
	ResetUploadFingerprintCache()
	tmp := t.TempDir()
	local := filepath.Join(tmp, "file.txt")
	if err := os.WriteFile(local, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	if _, err := RunOnceWithReport(context.Background(), "up", noopSyncClient{}, "ns", "pod", []Pair{{Local: local, Remote: "/workspace/file.txt"}}, nil); err != nil {
		t.Fatal(err)
	}
	stats, err := RunOnceWithOptions(context.Background(), "up", noopSyncClient{}, "ns", "pod", []Pair{{Local: local, Remote: "/workspace/file.txt"}}, nil, RunOptions{Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if stats.SkippedPaths != 0 {
		t.Fatalf("expected force sync to bypass cache, skipped=%d", stats.SkippedPaths)
	}
}
