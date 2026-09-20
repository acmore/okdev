package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

func TestForegroundForwardRetriesDNSSetupStreamLossAndReplacement(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	attempt := 0
	resolve := func(context.Context) (workload.TargetRef, error) {
		attempt++
		switch attempt {
		case 1:
			return workload.TargetRef{}, &net.DNSError{Name: "api.invalid", Err: "no such host"}
		case 3:
			return workload.TargetRef{}, fmt.Errorf("%w: replacement pending", ErrSessionNotFound)
		}
		return workload.TargetRef{PodName: fmt.Sprintf("pod-%d", attempt)}, nil
	}
	var targets []string
	forward := func(_ context.Context, target workload.TargetRef) error {
		targets = append(targets, target.PodName)
		if len(targets) == 1 {
			return errors.New("lost connection to pod")
		}
		cancel()
		return context.Canceled
	}
	var output bytes.Buffer
	err := recoverPortForward(ctx, resolve, forward, true, time.Millisecond, &output)
	if !errors.Is(err, context.Canceled) || !reflect.DeepEqual(targets, []string{"pod-2", "pod-4"}) {
		t.Fatalf("targets=%v err=%v", targets, err)
	}
	if strings.Count(output.String(), "retrying in") != 3 || !strings.Contains(output.String(), "local listeners are closed") {
		t.Fatalf("missing recovery diagnostics: %s", output.String())
	}
}

func TestForegroundForwardStopsOnTerminalErrors(t *testing.T) {
	for _, err := range []error{
		apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "x", errors.New("denied")),
		apierrors.NewUnauthorized("login required"),
		apierrors.NewBadRequest("malformed"),
		errors.New("unable to listen on any of the requested ports"),
		errors.New("address already in use"),
		errors.New("x509: certificate signed by unknown authority"),
		errors.New("invalid port mapping"),
	} {
		for _, resolving := range []bool{false, true} {
			if retryForegroundForward(err, resolving, true) {
				t.Errorf("retried terminal error %v (resolving=%v)", err, resolving)
			}
		}
	}
	missing := apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "old")
	if retryForegroundForward(missing, false, false) || !retryForegroundForward(missing, false, true) {
		t.Fatal("explicit pod disappearance must stop; automatic target may follow replacement")
	}
	if retryForegroundForward(errors.New("port-forward requires exactly one pod, got 2"), true, true) {
		t.Fatal("ambiguous selection is not recoverable")
	}
}

func TestForegroundForwardCancellationInterruptsBackoff(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	started := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- recoverPortForward(ctx, func(context.Context) (workload.TargetRef, error) {
			close(started)
			return workload.TargetRef{}, io.EOF
		}, nil, true, time.Hour, io.Discard)
	}()
	<-started
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation waited for backoff")
	}
}

func TestPortForwardRejectsOutOfRangePortsBeforeConnecting(t *testing.T) {
	for _, mapping := range []string{"65536:80", "80:65536"} {
		if _, err := parsePortForwardMappings([]string{mapping}); err == nil {
			t.Fatalf("accepted invalid mapping %s", mapping)
		}
	}
}

type forwardPodGetter func(context.Context, string, string) (*kube.PodSummary, error)

func (f forwardPodGetter) GetPodSummary(ctx context.Context, namespace, pod string) (*kube.PodSummary, error) {
	return f(ctx, namespace, pod)
}

func TestForegroundForwardDetectsIdlePodReplacement(t *testing.T) {
	for _, change := range []string{"deleted", "terminating", "same-name-new-UID", "forbidden"} {
		t.Run(change, func(t *testing.T) {
			calls := 0
			getter := forwardPodGetter(func(context.Context, string, string) (*kube.PodSummary, error) {
				calls++
				if calls == 1 {
					return &kube.PodSummary{UID: "original"}, nil
				}
				if calls == 2 {
					return nil, io.EOF // A single API interruption must not stop a healthy stream.
				}
				switch change {
				case "deleted":
					return nil, apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, "pod")
				case "terminating":
					return &kube.PodSummary{UID: "original", Deleting: true}, nil
				case "forbidden":
					return nil, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "pod", errors.New("denied"))
				default:
					return &kube.PodSummary{UID: "replacement"}, nil
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			err := forwardWhilePodExists(ctx, getter, "demo", "pod", time.Millisecond, func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			})
			if calls < 3 || (change == "forbidden" && !apierrors.IsForbidden(err)) || (change != "forbidden" && !apierrors.IsNotFound(err)) {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}

type blockedForwardAccess struct {
	sessionAccessReader
	started chan struct{}
}

func (b blockedForwardAccess) ListPods(ctx context.Context, _ string, _ bool, _ string) ([]kube.PodSummary, error) {
	close(b.started)
	<-ctx.Done()
	return nil, ctx.Err()
}

func TestForegroundForwardCancelsSessionLookup(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	for _, infer := range []bool{false, true} {
		t.Run(fmt.Sprintf("infer=%t", infer), func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			reader := blockedForwardAccess{started: make(chan struct{})}
			done := make(chan error, 1)
			go func() {
				if infer {
					_, err := resolveSessionNameWithReader(&Options{}, &config.DevEnvironment{}, "demo", true, reader, ctx)
					done <- err
				} else {
					done <- ensureSessionAccess(&Options{}, reader, "demo", "forward", true, ctx)
				}
			}()
			select {
			case <-reader.started:
			case <-time.After(time.Second):
				t.Fatal("session lookup did not start")
			}
			cancel()
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("session lookup ignored cancellation")
			}
		})
	}
}
