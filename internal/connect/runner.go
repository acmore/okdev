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
	return runExecWithRetry(ctx, stderr, policy, func() error {
		return client.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
	})
}

func runExecWithRetry(ctx context.Context, stderr io.Writer, policy RetryPolicy, execFn func() error) error {
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
		err := execFn()
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

// ExecContainerClient extends ExecClient with container-targeted exec.
type ExecContainerClient interface {
	ExecClient
	ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error
}

// RunOnContainer executes a command in a specific container within a pod using
// DefaultRetryPolicy. Transient connect errors are retried the same way as
// RunWithRetry; non-transient errors (including non-zero command exit codes)
// surface immediately. If container is empty, it falls back to the
// pod-level ExecInteractive path.
func RunOnContainer(ctx context.Context, client ExecClient, namespace, pod, container string, command []string, tty bool, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return RunOnContainerWithRetry(ctx, client, namespace, pod, container, command, tty, stdin, stdout, stderr, DefaultRetryPolicy)
}

// RunOnContainerWithRetry is the explicit-policy variant of RunOnContainer.
func RunOnContainerWithRetry(ctx context.Context, client ExecClient, namespace, pod, container string, command []string, tty bool, stdin io.Reader, stdout io.Writer, stderr io.Writer, policy RetryPolicy) error {
	if container == "" {
		return RunWithRetry(ctx, client, namespace, pod, command, tty, stdin, stdout, stderr, policy)
	}
	ec, ok := client.(ExecContainerClient)
	if !ok {
		return RunWithRetry(ctx, client, namespace, pod, command, tty, stdin, stdout, stderr, policy)
	}
	return runExecWithRetry(ctx, stderr, policy, func() error {
		return ec.ExecInteractiveInContainer(ctx, namespace, pod, container, tty, command, stdin, stdout, stderr)
	})
}

// IsTransientExecError reports whether err looks like a transport/network-level
// failure from a kube exec stream (broken pipe, EOF mid-stream, connection
// reset, etc.) rather than a non-zero command exit. It is the single source of
// truth for both the retry path in this package and failure classification in
// callers (e.g. multi-pod exec). Context cancellation and deadline-exceeded
// errors return false; callers that care about those should check them
// explicitly with errors.Is.
//
// The patterns are deliberately narrow substrings unique to transport failures.
// Bare "timeout" is intentionally excluded because remote command stderr (and
// wrapped error context) commonly contains the word in non-transport senses
// (e.g. "request timeout from upstream", "nslookup: timed out"); use
// "i/o timeout", "tls handshake timeout", or "client.timeout exceeded" instead.
func IsTransientExecError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	patterns := []string{
		"eof",
		"broken pipe",
		"connection reset",
		"connection refused",
		"connection closed",
		"closed by remote host",
		"i/o timeout",
		"tls handshake timeout",
		"client.timeout exceeded",
		"stream error",
		"transport is closing",
		"error reading from error stream",
		"use of closed network connection",
	}
	for _, p := range patterns {
		if strings.Contains(msg, p) {
			return true
		}
	}
	return false
}

func isRetryableError(err error) bool {
	return IsTransientExecError(err)
}
