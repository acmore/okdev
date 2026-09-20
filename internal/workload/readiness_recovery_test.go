package workload

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type recoveryWaitClient struct {
	podLister
	wait func(context.Context, string, time.Duration) error
}

func (c *recoveryWaitClient) WaitReadyWithProgress(ctx context.Context, _ string, pod string, timeout time.Duration, _ func(kube.PodReadinessProgress)) error {
	return c.wait(ctx, pod, timeout)
}

func TestReadinessRecoversAtEveryReadStage(t *testing.T) {
	for _, stage := range []string{"selection", "target watch", "remaining selection", "remaining watch"} {
		for _, transport := range []error{errors.New("http2: client connection lost"), io.ErrUnexpectedEOF, apierrors.NewServiceUnavailable("apiserver restarting"), apierrors.NewResourceExpired("old resource version"), &net.DNSError{Err: "temporary DNS failure", IsTemporary: true}, &net.OpError{Op: "read", Net: "tcp", Err: syscall.ECONNRESET}, apierrors.NewTooManyRequests("overloaded", 0)} {
			t.Run(stage+"/"+transport.Error(), func(t *testing.T) {
				selections, waits := 0, 0
				injected, workerReady := false, false
				client := &recoveryWaitClient{}
				client.wait = func(ctx context.Context, pod string, timeout time.Duration) error {
					waits++
					if !injected && ((stage == "target watch" && waits == 1) || (stage == "remaining watch" && pod == "worker")) {
						injected = true
						return transport
					}
					if pod == "worker" {
						workerReady = true
					}
					return nil
				}
				selectPod := func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
					selections++
					if !injected && ((stage == "selection" && selections == 1) || (stage == "remaining selection" && selections == 2)) {
						injected = true
						return TargetRef{}, nil, transport
					}
					ready := "0/1"
					if workerReady {
						ready = "1/1"
					}
					return TargetRef{PodName: "target"}, []kube.PodSummary{{Name: "target", Ready: "1/1"}, {Name: "worker", Ready: ready}}, nil
				}
				var progress []string
				err := waitForCandidatePodReady(context.Background(), client, "default", selectPod, 2*time.Second, func(p kube.PodReadinessProgress) { progress = append(progress, p.Reason) }, false, "timed out")
				if err != nil {
					t.Fatal(err)
				}
				if !injected || !workerReady {
					t.Fatalf("missed recovery: injected=%v workerReady=%v", injected, workerReady)
				}
				if !strings.Contains(strings.Join(progress, " "), "retrying") {
					t.Fatalf("missing reconnect progress: %q", progress)
				}
			})
		}
	}
}

func TestReadinessSelectionStopsOnTerminalErrors(t *testing.T) {
	for _, terminal := range []error{apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "target", errors.New("denied")), apierrors.NewUnauthorized("expired credentials"), errors.New("invalid kubeconfig"), &net.DNSError{Err: "no such host", IsNotFound: true}, apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "target", errors.New("http2: client connection lost"))} {
		t.Run(terminal.Error(), func(t *testing.T) {
			calls := 0
			selector := func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
				calls++
				return TargetRef{}, nil, terminal
			}
			err := waitForCandidatePodReady(context.Background(), &recoveryWaitClient{}, "default", selector, 600*time.Millisecond, nil, false, "timed out")
			if !errors.Is(err, terminal) {
				t.Fatalf("error = %v, want %v", err, terminal)
			}
			if calls != 1 {
				t.Fatalf("terminal error retried %d times", calls)
			}
		})
	}
}

func TestReadinessDeadlineBoundsSelectionAndBackoff(t *testing.T) {
	t.Run("blocked selection", func(t *testing.T) {
		parent, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		selector := func(ctx context.Context, _ podLister, _ string) (TargetRef, []kube.PodSummary, error) {
			<-ctx.Done()
			return TargetRef{}, nil, ctx.Err()
		}
		start := time.Now()
		err := waitForCandidatePodReady(parent, &recoveryWaitClient{}, "default", selector, 40*time.Millisecond, nil, false, "timed out")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v", err)
		}
		if elapsed := time.Since(start); elapsed > 400*time.Millisecond {
			t.Fatalf("selection exceeded requested deadline: %s", elapsed)
		}
	})
	t.Run("persistent transport failure", func(t *testing.T) {
		calls := 0
		selector := func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
			calls++
			return TargetRef{}, nil, errors.New("http2: client connection lost")
		}
		err := waitForCandidatePodReady(context.Background(), &recoveryWaitClient{}, "default", selector, 50*time.Millisecond, nil, false, "timed out")
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("error = %v, want deadline", err)
		}
		if calls > 2 {
			t.Fatalf("retry loop spun %d times", calls)
		}
	})
}

func TestReadinessCancellationStopsBeforeAnotherRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	calls := 0
	selector := func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
		calls++
		cancel()
		return TargetRef{}, nil, errors.New("pod/old is terminating")
	}
	err := waitForCandidatePodReady(ctx, &recoveryWaitClient{}, "default", selector, time.Second, nil, false, "timed out")
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("error=%v calls=%d", err, calls)
	}
}

func TestRemainingReadinessDoesNotSucceedAfterAllPodsDisappear(t *testing.T) {
	err := waitForRemainingPods(context.Background(), &recoveryWaitClient{}, "default", func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
		return TargetRef{}, nil, nil
	}, time.Now().Add(40*time.Millisecond), nil, false)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty receiver set reported success: %v", err)
	}
}

func TestReadinessReselectsReplacementPod(t *testing.T) {
	var waited []string
	client := &recoveryWaitClient{wait: func(_ context.Context, pod string, _ time.Duration) error {
		waited = append(waited, pod)
		if pod == "old" {
			return apierrors.NewNotFound(schema.GroupResource{Resource: "pods"}, pod)
		}
		return nil
	}}
	selector := func(context.Context, podLister, string) (TargetRef, []kube.PodSummary, error) {
		name := "old"
		if len(waited) > 0 {
			name = "new"
		}
		return TargetRef{PodName: name}, []kube.PodSummary{{Name: name, Ready: "1/1"}}, nil
	}
	if err := waitForCandidatePodReady(context.Background(), client, "default", selector, time.Second, nil, false, "timed out"); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(waited) != "[old new]" {
		t.Fatalf("waited on %v", waited)
	}
}
