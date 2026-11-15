// Package gpulease hands out whole GPUs to the commands of one pod.
//
// A lease is an exclusive flock on a per-GPU file. The lock belongs to the
// open file description, which the leased command inherits, so the GPU frees
// itself the moment the command and everything it started are gone — there is
// no release step to forget and nothing to clean up after a crash.
//
// Leases are advisory: a process started without one still sees every GPU. To
// keep such a process from being double-booked, a GPU that reports a compute
// process while nobody holds its lease counts as busy.
package gpulease

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// GPU identifies one device. UUID is what CUDA_VISIBLE_DEVICES is set to:
// CUDA's default enumeration order is not nvidia-smi's index order.
type GPU struct {
	Index int
	UUID  string
}

// Prober reports the pod's GPUs and which of them run a compute process.
type Prober interface {
	GPUs(ctx context.Context) ([]GPU, error)
	BusyUUIDs(ctx context.Context) (map[string]bool, error)
}

// Holder describes who holds a lease, for the report shown to whoever could
// not get one.
type Holder struct {
	Worktree string    `json:"worktree,omitempty"`
	JobID    string    `json:"jobId,omitempty"`
	PID      int       `json:"pid,omitempty"`
	Command  string    `json:"command,omitempty"`
	Since    time.Time `json:"since"`
}

func (h Holder) String() string {
	var parts []string
	if h.Worktree != "" {
		parts = append(parts, "worktree "+h.Worktree)
	}
	if h.JobID != "" {
		parts = append(parts, "job "+h.JobID)
	}
	if h.PID > 0 {
		parts = append(parts, fmt.Sprintf("pid %d", h.PID))
	}
	if h.Command != "" {
		parts = append(parts, h.Command)
	}
	if len(parts) == 0 {
		return "an okdev command"
	}
	return strings.Join(parts, ", ")
}

// BusyGPU is a GPU that could not be leased. Holder is nil when a process
// without a lease is using it.
type BusyGPU struct {
	GPU    GPU
	Holder *Holder
}

// ErrNoGPUs means the pod has no GPU at all.
var ErrNoGPUs = errors.New("no GPUs found in this container")

// TooFewError means the pod will never satisfy the request.
type TooFewError struct{ Want, Total int }

func (e *TooFewError) Error() string {
	return fmt.Sprintf("%d GPUs requested but this container has %d", e.Want, e.Total)
}

// UnavailableError means the request could be satisfied later, once some of
// the listed GPUs are free again.
type UnavailableError struct {
	Want, Free int
	Busy       []BusyGPU
}

func (e *UnavailableError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "%d GPUs requested, %d free", e.Want, e.Free)
	for _, busy := range e.Busy {
		who := "a process started without --gpus"
		if busy.Holder != nil {
			who = busy.Holder.String()
		}
		fmt.Fprintf(&b, "\n  gpu %d: %s", busy.GPU.Index, who)
	}
	return b.String()
}

// Lease is a set of GPUs held for the caller.
type Lease struct {
	GPUs  []GPU
	files []*os.File
}

// Files are the open lease files. Whoever keeps them open keeps the GPUs.
func (l *Lease) Files() []*os.File { return l.files }

// Release gives the GPUs back.
func (l *Lease) Release() {
	for _, f := range l.files {
		_ = f.Close()
	}
	l.files = nil
}

// pollInterval is how often a waiting Acquire looks again.
var pollInterval = time.Second

// Acquire leases want GPUs (all of them when want is 0), all at once or not
// at all: a caller never holds some GPUs while waiting for others, so two
// callers cannot deadlock each other. With wait > 0 it keeps trying for that
// long before returning the UnavailableError.
func Acquire(ctx context.Context, dir string, want int, wait time.Duration, holder Holder, prober Prober) (*Lease, error) {
	if err := os.MkdirAll(dir, 0o777); err != nil {
		return nil, fmt.Errorf("create GPU lease directory: %w", err)
	}
	// Every user of the pod leases from the same directory.
	_ = os.Chmod(dir, 0o777)
	deadline := time.Now().Add(wait)
	for {
		lease, err := tryAcquire(ctx, dir, want, holder, prober)
		var unavailable *UnavailableError
		if err == nil || !errors.As(err, &unavailable) || !time.Now().Before(deadline) {
			return lease, err
		}
		select {
		case <-ctx.Done():
			return nil, err
		case <-time.After(pollInterval):
		}
	}
}

func tryAcquire(ctx context.Context, dir string, want int, holder Holder, prober Prober) (*Lease, error) {
	gpus, err := prober.GPUs(ctx)
	if err != nil {
		return nil, err
	}
	if len(gpus) == 0 {
		return nil, ErrNoGPUs
	}
	sort.Slice(gpus, func(i, j int) bool { return gpus[i].Index < gpus[j].Index })
	if want == 0 {
		want = len(gpus)
	}
	if want > len(gpus) {
		return nil, &TooFewError{Want: want, Total: len(gpus)}
	}

	// One allocation at a time, so two requests that together exceed the
	// free GPUs cannot each grab a part and both fail.
	alloc, err := openLeaseFile(filepath.Join(dir, "alloc.lock"))
	if err != nil {
		return nil, err
	}
	defer alloc.Close()
	if err := syscall.Flock(int(alloc.Fd()), syscall.LOCK_EX); err != nil {
		return nil, fmt.Errorf("lock GPU allocation: %w", err)
	}

	// Without a reliable occupancy query, an unleased compute process could
	// receive a second allocation. Stop instead of assuming the GPUs are idle.
	inUse, err := prober.BusyUUIDs(ctx)
	if err != nil {
		return nil, fmt.Errorf("query GPU compute processes: %w", err)
	}

	lease := &Lease{}
	var busy []BusyGPU
	for _, gpu := range gpus {
		if len(lease.GPUs) == want {
			break
		}
		f, err := openLeaseFile(filepath.Join(dir, fmt.Sprintf("gpu%d.lease", gpu.Index)))
		if err != nil {
			lease.Release()
			return nil, err
		}
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
			busy = append(busy, BusyGPU{GPU: gpu, Holder: readHolder(f)})
			_ = f.Close()
			continue
		}
		if inUse[gpu.UUID] {
			busy = append(busy, BusyGPU{GPU: gpu})
			_ = f.Close()
			continue
		}
		lease.GPUs = append(lease.GPUs, gpu)
		lease.files = append(lease.files, f)
	}
	if len(lease.GPUs) < want {
		free := len(lease.GPUs)
		lease.Release()
		return nil, &UnavailableError{Want: want, Free: free, Busy: busy}
	}
	for _, f := range lease.files {
		writeHolder(f, holder)
	}
	return lease, nil
}

func openLeaseFile(path string) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o666)
	if err != nil {
		return nil, fmt.Errorf("open GPU lease %s: %w", path, err)
	}
	// Other users of the pod must be able to lease and to read the holder.
	_ = f.Chmod(0o666)
	return f, nil
}

func writeHolder(f *os.File, holder Holder) {
	raw, err := json.Marshal(holder)
	if err != nil {
		return
	}
	if err := f.Truncate(0); err != nil {
		return
	}
	_, _ = f.WriteAt(append(raw, '\n'), 0)
}

func readHolder(f *os.File) *Holder {
	raw := make([]byte, 4096)
	n, _ := f.ReadAt(raw, 0)
	var holder Holder
	if n == 0 || json.Unmarshal(raw[:n], &holder) != nil {
		return &Holder{}
	}
	return &holder
}

// VisibleDevices is the CUDA_VISIBLE_DEVICES value for the leased GPUs.
func (l *Lease) VisibleDevices() string {
	uuids := make([]string, 0, len(l.GPUs))
	for _, gpu := range l.GPUs {
		uuids = append(uuids, gpu.UUID)
	}
	return strings.Join(uuids, ",")
}

// Indices is the leased GPUs' nvidia-smi indices, comma separated.
func (l *Lease) Indices() string {
	indices := make([]string, 0, len(l.GPUs))
	for _, gpu := range l.GPUs {
		indices = append(indices, fmt.Sprint(gpu.Index))
	}
	return strings.Join(indices, ",")
}
