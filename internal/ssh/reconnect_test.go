package ssh

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestReconnectWithBackoffSucceedsAfterRetries(t *testing.T) {
	t.Parallel()
	var calls int
	err := reconnectWithBackoff(context.Background(), func() error {
		calls++
		if calls < 3 {
			return errors.New("not ready")
		}
		return nil
	}, 5, 5*time.Millisecond, 20*time.Millisecond)
	if err != nil {
		t.Fatalf("unexpected reconnect error: %v", err)
	}
	if calls != 3 {
		t.Fatalf("unexpected call count: got %d want 3", calls)
	}
}

func TestReconnectWithBackoffStopsAfterMaxRetries(t *testing.T) {
	t.Parallel()
	var calls int
	err := reconnectWithBackoff(context.Background(), func() error {
		calls++
		return errors.New("still failing")
	}, 2, 5*time.Millisecond, 20*time.Millisecond)
	if err == nil {
		t.Fatal("expected reconnect error")
	}
	if calls != 2 {
		t.Fatalf("unexpected call count: got %d want 2", calls)
	}
}
