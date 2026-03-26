package cli

import (
	"context"
	"testing"
	"time"
)

func TestUpCommandContextUsesDefaultFloor(t *testing.T) {
	ctx, cancel := upCommandContext(30 * time.Second)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context deadline")
	}
	remaining := time.Until(deadline)
	if remaining < 4*time.Minute || remaining > 5*time.Minute+5*time.Second {
		t.Fatalf("expected default timeout near 5m, got %s", remaining)
	}
}

func TestUpCommandContextExpandsForLargeWaitTimeout(t *testing.T) {
	ctx, cancel := upCommandContext(10 * time.Minute)
	defer cancel()

	deadline, ok := ctx.Deadline()
	if !ok {
		t.Fatal("expected context deadline")
	}
	remaining := time.Until(deadline)
	expected := (2 * 10 * time.Minute) + (2 * time.Minute)
	if remaining < expected-5*time.Second || remaining > expected+5*time.Second {
		t.Fatalf("expected timeout near %s, got %s", expected, remaining)
	}
}

func TestUpCommandContextIsCancellable(t *testing.T) {
	ctx, cancel := upCommandContext(time.Minute)
	cancel()

	select {
	case <-ctx.Done():
	default:
		t.Fatal("expected cancelled context to be done")
	}
	if ctx.Err() != context.Canceled {
		t.Fatalf("expected context canceled, got %v", ctx.Err())
	}
}
