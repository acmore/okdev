package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestExecPreflightRetriesReadsWithinBudget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	reader := &flakySessionAccessReader{errs: []error{io.EOF, io.EOF, io.EOF}, pods: []kube.PodSummary{{Name: "pod"}}}
	var delays []time.Duration
	pods, err := listSessionPodsWithRetry(context.Background(), reader, "ns", "test", 4, time.Millisecond, 2*time.Millisecond, func(_ int, d time.Duration) { delays = append(delays, d) })
	if err != nil || len(pods) != 1 || reader.calls != 4 {
		t.Fatalf("pods=%v err=%v calls=%d", pods, err, reader.calls)
	}
	if len(delays) != 3 || delays[0] != time.Millisecond || delays[1] != 2*time.Millisecond || delays[2] != 2*time.Millisecond {
		t.Fatalf("backoff=%v", delays)
	}
}

func TestExecPreflightBudgetAndPermanentFailure(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	var out bytes.Buffer
	reader := &flakySessionAccessReader{errs: []error{io.EOF}}
	err := ensureExecSessionAccess(context.Background(), &Options{}, reader, "ns", "test", 20*time.Millisecond, &out)
	if !errors.Is(err, ErrTransientCluster) || reader.calls != 1 || !strings.Contains(err.Error(), "budget") {
		t.Fatalf("err=%v calls=%d", err, reader.calls)
	}
	if !strings.Contains(out.String(), "command not sent") {
		t.Fatal(out.String())
	}
	reader = &flakySessionAccessReader{errs: []error{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "test", errors.New("denied"))}}
	out.Reset()
	err = ensureExecSessionAccess(context.Background(), &Options{}, reader, "ns", "test", time.Second, &out)
	if !apierrors.IsForbidden(err) || reader.calls != 1 || out.Len() != 0 {
		t.Fatalf("err=%v calls=%d output=%q", err, reader.calls, out.String())
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	reader = &flakySessionAccessReader{}
	err = ensureExecSessionAccess(ctx, &Options{}, reader, "ns", "test", time.Second, &out)
	if !errors.Is(err, context.Canceled) || reader.calls != 0 {
		t.Fatalf("err=%v calls=%d", err, reader.calls)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	reader = &flakySessionAccessReader{errs: []error{io.EOF}}
	err = ensureExecSessionAccess(ctx, &Options{}, reader, "ns", "test", time.Hour, &out)
	if !errors.Is(err, context.DeadlineExceeded) || reader.calls != 1 {
		t.Fatalf("err=%v calls=%d", err, reader.calls)
	}
}

func TestExecCommandDeliveryNeverReplaysAmbiguousFailure(t *testing.T) {
	pods := []kube.PodSummary{{Name: "pod"}}
	for _, mode := range []string{"stream", "json", "script", "detach"} {
		t.Run(mode, func(t *testing.T) {
			client := &fakePdshExecClient{outputs: map[string]string{"pod": "command already started\n"}, errs: map[string]error{"pod": io.ErrUnexpectedEOF}}
			invocation := execInvocation{Argv: []string{"non-idempotent-command"}}
			var out, errOut bytes.Buffer
			sem := make(chan struct{}, 1)
			switch mode {
			case "stream":
				_ = runMultiExec(context.Background(), client, "ns", pods, "dev", invocation.Argv, "", 0, true, sem, &out, &errOut)
			case "json":
				_ = runMultiExecJSON(context.Background(), client, "ns", pods, "dev", invocation, 0, 1, &out)
			case "script":
				invocation.ScriptLocalPath = "test.sh"
				_ = runMultiExecScript(context.Background(), client, "ns", pods, "dev", invocation, "", 0, true, sem, &out, &errOut)
			case "detach":
				_ = runDetachExec(context.Background(), client, "ns", pods, "dev", invocation, false, sem, &out)
			}
			if len(client.calls) != 1 {
				t.Fatalf("ambiguous failure replayed command %d times", len(client.calls))
			}
		})
	}
}
