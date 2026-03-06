package connect

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

type ExecClient interface {
	ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
}

type RetryPolicy struct {
	MaxAttempts    int
	InitialBackoff time.Duration
	MaxBackoff     time.Duration
}

var DefaultRetryPolicy = RetryPolicy{
	MaxAttempts:    3,
	InitialBackoff: 1 * time.Second,
	MaxBackoff:     5 * time.Second,
}

func Run(ctx context.Context, client ExecClient, namespace, pod string, command []string, tty bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return RunWithRetry(ctx, client, namespace, pod, command, tty, stdin, stdout, stderr, DefaultRetryPolicy)
}

func RunWithRetry(ctx context.Context, client ExecClient, namespace, pod string, command []string, tty bool, stdin io.Reader, stdout io.Writer, stderr io.Writer, policy RetryPolicy) error {
	if policy.MaxAttempts <= 0 {
		policy.MaxAttempts = 1
	}
	if policy.InitialBackoff <= 0 {
		policy.InitialBackoff = 500 * time.Millisecond
	}
	if policy.MaxBackoff <= 0 {
		policy.MaxBackoff = 5 * time.Second
	}

	backoff := policy.InitialBackoff
	var lastErr error
	for attempt := 1; attempt <= policy.MaxAttempts; attempt++ {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		err := client.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
		if err == nil {
			return nil
		}
		lastErr = err
		if !isRetryableError(err) || attempt == policy.MaxAttempts {
			return err
		}

		fmt.Fprintf(stderr, "connect error: %v (retry %d/%d in %s)\n", err, attempt, policy.MaxAttempts, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < policy.MaxBackoff {
			backoff *= 2
			if backoff > policy.MaxBackoff {
				backoff = policy.MaxBackoff
			}
		}
	}
	if lastErr != nil {
		return lastErr
	}
	return nil
}

func isRetryableError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"eof",
		"connection reset",
		"broken pipe",
		"timeout",
		"i/o timeout",
		"stream error",
		"connection refused",
		"transport is closing",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}
