package gpulease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type fakeProber struct {
	gpus    []GPU
	busy    map[string]bool
	busyErr error
}

func (p *fakeProber) GPUs(context.Context) ([]GPU, error) { return p.gpus, nil }
func (p *fakeProber) BusyUUIDs(context.Context) (map[string]bool, error) {
	return p.busy, p.busyErr
}

func fourGPUs() *fakeProber {
	return &fakeProber{gpus: []GPU{{0, "GPU-a"}, {1, "GPU-b"}, {2, "GPU-c"}, {3, "GPU-d"}}}
}

func TestLeasesAreExclusiveAndAllOrNothing(t *testing.T) {
	dir := t.TempDir()
	ctx := context.Background()
	prober := fourGPUs()

	first, err := Acquire(ctx, dir, 3, 0, Holder{Worktree: "wt-a", JobID: "job-1"}, prober)
	if err != nil {
		t.Fatal(err)
	}
	if first.Indices() != "0,1,2" || first.VisibleDevices() != "GPU-a,GPU-b,GPU-c" {
		t.Fatalf("first lease = %s / %s", first.Indices(), first.VisibleDevices())
	}

	// Two are wanted, one is free: the request fails whole and keeps nothing.
	_, err = Acquire(ctx, dir, 2, 0, Holder{Worktree: "wt-b"}, prober)
	var unavailable *UnavailableError
	if !errors.As(err, &unavailable) || unavailable.Free != 1 || len(unavailable.Busy) != 3 {
		t.Fatalf("second request = %v", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "worktree wt-a") || !strings.Contains(msg, "job job-1") {
		t.Fatalf("holders not reported: %s", msg)
	}
	one, err := Acquire(ctx, dir, 1, 0, Holder{}, prober)
	if err != nil || one.Indices() != "3" {
		t.Fatalf("the free GPU was kept by the failed request: %v, %v", one, err)
	}
	one.Release()

	first.Release()
	all, err := Acquire(ctx, dir, 0, 0, Holder{}, prober)
	if err != nil || all.Indices() != "0,1,2,3" {
		t.Fatalf("all GPUs after release = %v, %v", all, err)
	}
	all.Release()
}

func TestRequestsThatCanNeverBeMetFailAtOnce(t *testing.T) {
	ctx := context.Background()
	var tooFew *TooFewError
	if _, err := Acquire(ctx, t.TempDir(), 5, time.Hour, Holder{}, fourGPUs()); !errors.As(err, &tooFew) {
		t.Fatalf("oversized request = %v", err)
	}
	if _, err := Acquire(ctx, t.TempDir(), 1, time.Hour, Holder{}, &fakeProber{}); !errors.Is(err, ErrNoGPUs) {
		t.Fatalf("request without GPUs = %v", err)
	}
}

// A process that never leased its GPU must not be double-booked.
func TestGPUUsedWithoutALeaseIsBusy(t *testing.T) {
	prober := fourGPUs()
	prober.busy = map[string]bool{"GPU-a": true}
	lease, err := Acquire(context.Background(), t.TempDir(), 2, 0, Holder{}, prober)
	if err != nil || lease.Indices() != "1,2" {
		t.Fatalf("lease = %v, %v", lease, err)
	}
	lease.Release()
	_, err = Acquire(context.Background(), t.TempDir(), 4, 0, Holder{}, prober)
	if err == nil || !strings.Contains(err.Error(), "gpu 0: a process started without --gpus") {
		t.Fatalf("unleased use not reported: %v", err)
	}
}

func TestOccupancyProbeFailureDoesNotLeaseGPU(t *testing.T) {
	prober := fourGPUs()
	prober.busyErr = errors.New("compute process query failed")
	dir := t.TempDir()
	if lease, err := Acquire(context.Background(), dir, 1, time.Minute, Holder{}, prober); lease != nil || err == nil || !strings.Contains(err.Error(), "compute process query failed") {
		t.Fatalf("probe failure: lease = %v, err = %v", lease, err)
	}
	prober.busyErr = nil
	lease, err := Acquire(context.Background(), dir, 1, 0, Holder{}, prober)
	if err != nil {
		t.Fatalf("probe recovery: %v", err)
	}
	lease.Release()
}

func TestWaitingRequestGetsTheGPUWhenItIsFreed(t *testing.T) {
	old := pollInterval
	pollInterval = 10 * time.Millisecond
	t.Cleanup(func() { pollInterval = old })
	dir := t.TempDir()
	prober := fourGPUs()
	held, err := Acquire(context.Background(), dir, 0, 0, Holder{}, prober)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := Acquire(context.Background(), dir, 1, 30*time.Millisecond, Holder{}, prober); err == nil {
		t.Fatal("wait elapsed without an error")
	}
	go func() {
		time.Sleep(30 * time.Millisecond)
		held.Release()
	}()
	lease, err := Acquire(context.Background(), dir, 2, 5*time.Second, Holder{}, prober)
	if err != nil || lease.Indices() != "0,1" {
		t.Fatalf("waiting request = %v, %v", lease, err)
	}
	lease.Release()
}
