package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

// Only this read-only ListPods request is replayed. No exec stream, script
// upload, detach launch or GPU reset is inside the retry boundary.
func listSessionPodsWithRetry(parent context.Context, k sessionAccessReader, namespace, sessionName string, attempts int, delay, maxDelay time.Duration, retry func(int, time.Duration)) ([]kube.PodSummary, error) {
	selector := "okdev.io/managed=true,okdev.io/session=" + sessionName
	for attempt := 1; ; attempt++ {
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		ctx, cancel := context.WithTimeout(parent, sessionAccessTimeout)
		pods, err := k.ListPods(ctx, namespace, false, selector)
		cancel()
		if parent.Err() != nil {
			return nil, parent.Err()
		}
		if err == nil {
			if clearErr := session.ClearTransientStreak(sessionName); clearErr != nil {
				slog.Debug("failed to clear transient streak", "session", sessionName, "error", clearErr)
			}
			return pods, nil
		}
		if !isTransientClusterError(err) && !errors.Is(err, io.EOF) {
			return nil, err
		}
		if attempts > 0 && attempt >= attempts {
			return nil, transientClusterFailure(sessionName, err, time.Now())
		}
		if retry != nil {
			retry(attempt, delay)
		}
		timer := time.NewTimer(delay)
		select {
		case <-parent.Done():
			timer.Stop()
			return nil, parent.Err()
		case <-timer.C:
		}
		if delay < maxDelay {
			delay *= 2
			if delay > maxDelay {
				delay = maxDelay
			}
		}
	}
}

func ensureExecSessionAccess(parent context.Context, opts *Options, k sessionAccessReader, namespace, name string, budget time.Duration, errOut io.Writer) error {
	if budget == 0 {
		return ensureSessionAccess(opts, k, namespace, name, true, parent)
	}
	ctx, cancel := context.WithTimeout(parent, budget)
	defer cancel()
	pods, err := listSessionPodsWithRetry(ctx, k, namespace, name, 0, sessionAccessRetryDelay, 5*time.Second, func(attempt int, delay time.Duration) {
		fmt.Fprintf(errOut, "exec preflight: transient cluster contact failure; retry %d in %s (budget %s; command not sent)\n", attempt, delay, budget)
	})
	if err != nil {
		if parent.Err() != nil {
			return parent.Err()
		}
		if ctx.Err() != nil {
			return transientClusterFailure(name, fmt.Errorf("exec preflight retry budget %s exhausted: %w", budget, ctx.Err()), time.Now())
		}
		return err
	}
	return checkSessionAccessPods(opts, namespace, name, true, pods)
}
