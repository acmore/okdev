package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
)

const lifecycleOutputLimit = 32 * 1024

type lifecycleExecClient interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

type lifecycleStreamClient interface {
	StreamShInContainer(context.Context, string, string, string, string, io.Writer, io.Writer) error
}

func executeLifecycleHook(ctx context.Context, k lifecycleExecClient, namespace, pod, container, hook, command string, out io.Writer) error {
	var captured lifecycleOutputTail
	var err error
	if streamer, ok := k.(lifecycleStreamClient); ok {
		err = streamer.StreamShInContainer(ctx, namespace, pod, container, command, &captured, &captured)
	} else {
		var output []byte
		output, err = k.ExecShInContainer(ctx, namespace, pod, container, command)
		_, _ = captured.Write(output)
	}
	var report strings.Builder
	if captured.truncated {
		fmt.Fprintf(&report, "[%s pod=%s] output truncated to last %d bytes\n", hook, pod, lifecycleOutputLimit)
	}
	text := strings.TrimSuffix(string(captured.data), "\n")
	if text != "" {
		for _, line := range strings.Split(text, "\n") {
			fmt.Fprintf(&report, "[%s pod=%s] %s\n", hook, pod, line)
		}
	}
	if report.Len() > 0 {
		_, _ = io.WriteString(out, report.String())
	}
	return err
}

// stdout and stderr can arrive concurrently; retain a bounded combined tail.
type lifecycleOutputTail struct {
	mu        sync.Mutex
	data      []byte
	truncated bool
}

func (b *lifecycleOutputTail) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(b.data)+n > lifecycleOutputLimit {
		b.truncated = true
		if n >= lifecycleOutputLimit {
			b.data = append(b.data[:0], p[n-lifecycleOutputLimit:]...)
			return n, nil
		}
		b.data = b.data[len(b.data)+n-lifecycleOutputLimit:]
	}
	b.data = append(b.data, p...)
	return n, nil
}

func lifecycleExitSummary(ran, skipped int) string {
	parts := []string{fmt.Sprintf("hook exited 0 on %d pod(s)", ran)}
	if skipped > 0 {
		parts = append(parts, fmt.Sprintf("previous exit 0 recorded on %d pod(s)", skipped))
	}
	return strings.Join(parts, "; ") + "; setup effects are not independently verified"
}

type lifecycleOutputSink struct{ ui *upUI }

func (s lifecycleOutputSink) Write(p []byte) (int, error) {
	s.ui.mu.Lock()
	defer s.ui.mu.Unlock()
	s.ui.stopActiveLocked()
	return s.ui.errOut.Write(p)
}
