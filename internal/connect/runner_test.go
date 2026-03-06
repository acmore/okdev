package connect

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type fakeExecClient struct {
	calls int
	errs  []error
}

func (f *fakeExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	if f.calls >= len(f.errs) {
		return nil
	}
	err := f.errs[f.calls]
	f.calls++
	return err
}

func TestRunWithRetrySucceedsAfterTransient(t *testing.T) {
	fc := &fakeExecClient{errs: []error{errors.New("EOF"), nil}}
	var out bytes.Buffer
	err := RunWithRetry(context.Background(), fc, "ns", "pod", []string{"sh"}, true, &out, &out, &out, RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	if fc.calls < 2 {
		t.Fatalf("expected retry, got calls=%d", fc.calls)
	}
}

func TestRunWithRetryRespectsCancel(t *testing.T) {
	fc := &fakeExecClient{errs: []error{errors.New("i/o timeout"), errors.New("i/o timeout")}}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	err := RunWithRetry(ctx, fc, "ns", "pod", []string{"sh"}, true, &out, &out, &out, RetryPolicy{
		MaxAttempts:    10,
		InitialBackoff: 50 * time.Millisecond,
		MaxBackoff:     50 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected cancellation error")
	}
	if time.Since(start) > time.Second {
		t.Fatalf("expected fast cancel, took %s", time.Since(start))
	}
}

func TestRunWithRetryNoRetryOnNonTransient(t *testing.T) {
	fc := &fakeExecClient{errs: []error{errors.New("permission denied")}}
	var out bytes.Buffer
	err := RunWithRetry(context.Background(), fc, "ns", "pod", []string{"sh"}, true, &out, &out, &out, RetryPolicy{
		MaxAttempts:    3,
		InitialBackoff: 1 * time.Millisecond,
		MaxBackoff:     2 * time.Millisecond,
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if fc.calls != 1 {
		t.Fatalf("expected no retry, got calls=%d", fc.calls)
	}
}
