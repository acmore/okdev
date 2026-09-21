package kube

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCopyEarlyRejectionCancelsBlockedInputWithoutRetry(t *testing.T) {
	old := runPodUploadExecForCopy
	t.Cleanup(func() { runPodUploadExecForCopy = old })
	calls := 0
	runPodUploadExecForCopy = func(ctx context.Context, _ *Client, _, _, _, _ string, _ io.Reader, stderr io.Writer) error {
		calls++
		_, _ = io.WriteString(stderr, "okdev-cp-")
		if ctx.Err() != nil {
			t.Fatal("partial marker cancelled prematurely")
		}
		_, _ = io.WriteString(stderr, "err: mktemp failed\n")
		if ctx.Err() == nil {
			t.Fatal("remote rejection did not cancel blocked stdin")
		}
		return ctx.Err()
	}
	source := filepath.Join(t.TempDir(), "data")
	if err := os.WriteFile(source, []byte("payload"), 0600); err != nil {
		t.Fatal(err)
	}
	err := (&Client{}).CopyToPodInContainerWithProgress(context.Background(), "ns", source, "pod", "dev", "/denied", CopyProgress{})
	if err == nil || !strings.Contains(err.Error(), "mktemp failed") || calls != 1 {
		t.Fatalf("err=%v attempts=%d", err, calls)
	}
}

func TestUploadAcknowledgementDoesNotCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer
	writer := &uploadErrorWriter{out: &out, cancel: cancel}
	_, _ = io.WriteString(writer, "okdev-cp-ok:123\n")
	if ctx.Err() != nil || out.String() != "okdev-cp-ok:123\n" {
		t.Fatal("successful acknowledgement changed")
	}
}
