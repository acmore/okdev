package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

type hookOutputClient struct {
	lifecycleExecClient
	stdout string
	stderr string
	err    error
}

func (k hookOutputClient) StreamShInContainer(_ context.Context, _, _, _, _ string, stdout, stderr io.Writer) error {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = io.WriteString(stdout, k.stdout) }()
	go func() { defer wg.Done(); _, _ = io.WriteString(stderr, k.stderr) }()
	wg.Wait()
	return k.err
}

func TestLifecycleOutputPreservesBothStreamsAndFailure(t *testing.T) {
	failure := errors.New("remote exit 9")
	for _, remoteErr := range []error{nil, failure} {
		var out bytes.Buffer
		err := executeLifecycleHook(context.Background(), hookOutputClient{stdout: "checking\n", stderr: "missing file\n", err: remoteErr}, "demo", "worker-1", "dev", "postSync", "setup", &out)
		if !errors.Is(err, remoteErr) {
			t.Fatalf("lost command result: %v", err)
		}
		for _, line := range []string{"[postSync pod=worker-1] checking\n", "[postSync pod=worker-1] missing file\n"} {
			if !strings.Contains(out.String(), line) {
				t.Fatalf("missing attributed output %q in %q", line, out.String())
			}
		}
	}
}

func TestLifecycleOutputDoesNotRejectSilentSuccess(t *testing.T) {
	var out bytes.Buffer
	if err := executeLifecycleHook(context.Background(), hookOutputClient{}, "demo", "pod", "dev", "postCreate", "true", &out); err != nil || out.Len() != 0 {
		t.Fatalf("silent success changed: output=%q err=%v", out.String(), err)
	}
}

func TestLifecycleOutputTailIsBounded(t *testing.T) {
	var captured lifecycleOutputTail
	_, _ = captured.Write(bytes.Repeat([]byte("a"), lifecycleOutputLimit*3))
	_, _ = captured.Write([]byte("last diagnostic\n"))
	if len(captured.data) != lifecycleOutputLimit || !captured.truncated || !bytes.HasSuffix(captured.data, []byte("last diagnostic\n")) {
		t.Fatalf("tail length=%d truncated=%t", len(captured.data), captured.truncated)
	}
	var out bytes.Buffer
	_ = executeLifecycleHook(context.Background(), hookOutputClient{stdout: strings.Repeat("x", lifecycleOutputLimit+1)}, "demo", "pod", "dev", "postCreate", "setup", &out)
	if !strings.Contains(out.String(), "[postCreate pod=pod] output truncated to last 32768 bytes") {
		t.Fatal("missing truncation notice")
	}
}

func TestLifecycleOutputIsImmediateStderrNotAWarning(t *testing.T) {
	var stdout, stderr bytes.Buffer
	ui := newUpUI(&stdout, &stderr)
	_, _ = (lifecycleOutputSink{ui: ui}).Write([]byte("[postSync pod=worker] checked\n"))
	if stdout.Len() != 0 || stderr.String() != "[postSync pod=worker] checked\n" || len(ui.warnings) != 0 {
		t.Fatalf("stdout=%q stderr=%q warnings=%v", stdout.String(), stderr.String(), ui.warnings)
	}
}
