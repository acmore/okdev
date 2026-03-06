package ports

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"
	"time"
)

type fakeClient struct {
	calls int
}

func (f *fakeClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	f.calls++
	if f.calls < 2 {
		return errors.New("boom")
	}
	return nil
}

type alwaysFailClient struct{}

func (a *alwaysFailClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	return errors.New("always fail")
}

func TestForwardWithRetry(t *testing.T) {
	fc := &fakeClient{}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var out bytes.Buffer
	if err := ForwardWithRetry(ctx, fc, "ns", "pod", []string{"8080:8080"}, &out, &out, 10*time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if fc.calls < 2 {
		t.Fatalf("expected retry, calls=%d", fc.calls)
	}
}

func TestForwardWithRetryRespectsContextCancel(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	var out bytes.Buffer
	start := time.Now()
	err := ForwardWithRetry(ctx, &alwaysFailClient{}, "ns", "pod", []string{"8080:8080"}, &out, &out, 30*time.Second)
	if err == nil {
		t.Fatal("expected context cancellation error")
	}
	if time.Since(start) > 2*time.Second {
		t.Fatalf("expected quick cancellation, took %s", time.Since(start))
	}
}
