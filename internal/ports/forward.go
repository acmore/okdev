package ports

import (
	"context"
	"fmt"
	"io"
	"time"
)

type PortForwardClient interface {
	PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error
}

func ForwardWithRetry(ctx context.Context, client PortForwardClient, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer, maxBackoff time.Duration) error {
	if maxBackoff <= 0 {
		maxBackoff = 30 * time.Second
	}
	backoff := 1 * time.Second
	const stableRunReset = 30 * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		start := time.Now()
		err := client.PortForward(ctx, namespace, pod, forwards, stdout, stderr)
		if err == nil {
			return nil
		}
		if time.Since(start) >= stableRunReset {
			backoff = 1 * time.Second
		}
		fmt.Fprintf(stderr, "port-forward error: %v (retrying in %s)\n", err, backoff)
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
		if backoff < maxBackoff {
			backoff *= 2
			if backoff > maxBackoff {
				backoff = maxBackoff
			}
		}
	}
}
