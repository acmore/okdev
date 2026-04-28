package cli

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// countingReader wraps an io.Reader and reports each successfully-read byte
// count to onBytes. onBytes may be nil.
type countingReader struct {
	r       io.Reader
	onBytes func(int64)
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 && c.onBytes != nil {
		c.onBytes(int64(n))
	}
	return n, err
}

// countingWriter wraps an io.Writer and reports each successfully-written byte
// count to onBytes. onBytes may be nil.
type countingWriter struct {
	w       io.Writer
	onBytes func(int64)
}

func (c *countingWriter) Write(p []byte) (int, error) {
	n, err := c.w.Write(p)
	if n > 0 && c.onBytes != nil {
		c.onBytes(int64(n))
	}
	return n, err
}

// cpProgress renders a single-line transient status for `okdev cp`. It is
// safe for use from multiple goroutines: addBytes/addFile use atomics; the
// in-flight set is mutex-protected.
//
// In non-interactive writers the progress line is suppressed entirely. The
// addBytes/addFile/startPod/finishPod calls remain safe no-ops in that mode.
type cpProgress struct {
	out     io.Writer
	prefix  string // single-pod message prefix; empty for multi-pod
	enabled bool

	bytes atomic.Int64
	files atomic.Int64

	multi bool
	total int
	done  atomic.Int32

	inFlightMu sync.Mutex
	inFlight   map[string]time.Time

	// Rolling-rate sampling. lastSample / lastBytes are advanced each tick
	// inside sampleRate, which may run from the ticker goroutine only.
	rateMu     sync.Mutex
	lastSample time.Time
	lastBytes  int64
	rateBPS    int64

	started time.Time

	status *transientStatus
	stopCh chan struct{}
	doneCh chan struct{}
}

const (
	cpProgressTickInterval    = statusSpinnerInterval
	cpProgressRateWindow      = 1 * time.Second
	cpProgressSlowMinElapsed  = 10 * time.Second
	cpProgressSlowFactor      = 2.0
	cpProgressElapsedShowFrom = statusElapsedThreshold
)

func newSinglePodProgress(out io.Writer, message string) *cpProgress {
	now := time.Now()
	return &cpProgress{
		out:        out,
		prefix:     message,
		enabled:    isInteractiveWriter(out),
		started:    now,
		lastSample: now,
		inFlight:   map[string]time.Time{},
	}
}

func newMultiPodProgress(out io.Writer, total int) *cpProgress {
	now := time.Now()
	return &cpProgress{
		out:        out,
		multi:      true,
		total:      total,
		enabled:    isInteractiveWriter(out),
		started:    now,
		lastSample: now,
		inFlight:   map[string]time.Time{},
	}
}

func (p *cpProgress) addBytes(n int64) {
	if p == nil || n <= 0 {
		return
	}
	p.bytes.Add(n)
}

func (p *cpProgress) addFile() {
	if p == nil {
		return
	}
	p.files.Add(1)
}

func (p *cpProgress) startPod(pod string) {
	if p == nil {
		return
	}
	p.inFlightMu.Lock()
	p.inFlight[pod] = time.Now()
	p.inFlightMu.Unlock()
}

func (p *cpProgress) finishPod(pod string) {
	if p == nil {
		return
	}
	p.inFlightMu.Lock()
	delete(p.inFlight, pod)
	p.inFlightMu.Unlock()
	p.done.Add(1)
}

// start begins the ticker goroutine that updates the transient status line.
// On non-interactive writers it is a cheap no-op.
func (p *cpProgress) start() {
	if p == nil || !p.enabled {
		return
	}
	p.status = newTransientStatusWithMode(p.out, p.currentMessage(time.Now()), true)
	if !p.status.enabled {
		p.status = nil
		p.enabled = false
		return
	}
	p.stopCh = make(chan struct{})
	p.doneCh = make(chan struct{})
	go p.tickLoop()
}

func (p *cpProgress) tickLoop() {
	defer close(p.doneCh)
	t := time.NewTicker(cpProgressTickInterval)
	defer t.Stop()
	for {
		select {
		case now := <-t.C:
			p.sampleRate(now)
			p.status.update(p.currentMessage(now))
		case <-p.stopCh:
			return
		}
	}
}

func (p *cpProgress) stop() {
	if p == nil || !p.enabled {
		return
	}
	close(p.stopCh)
	<-p.doneCh
	p.status.stop()
}

// println prints a persistent line through the spinner. When the spinner is
// active it clears the spinner line first so the persistent output is not
// garbled; the spinner is re-rendered on the next tick. When the spinner is
// disabled (non-TTY) it falls back to a plain Fprintln on out.
func (p *cpProgress) println(line string) {
	if p == nil {
		return
	}
	if p.enabled && p.status != nil {
		p.status.printAbove(line)
		return
	}
	fmt.Fprintln(p.out, line)
}

// sampleRate computes a rolling-window bytes-per-second value based on the
// difference since the last sample. Called from the ticker goroutine only,
// so the rate state does not need locking against itself, but rateBPS is
// also read by currentMessage from the same goroutine — the mutex guards
// against future callers reading it externally.
func (p *cpProgress) sampleRate(now time.Time) {
	p.rateMu.Lock()
	defer p.rateMu.Unlock()
	dt := now.Sub(p.lastSample)
	if dt < cpProgressRateWindow/4 {
		return
	}
	cur := p.bytes.Load()
	delta := cur - p.lastBytes
	if delta < 0 {
		delta = 0
	}
	p.rateBPS = int64(float64(delta) / dt.Seconds())
	p.lastSample = now
	p.lastBytes = cur
}

func (p *cpProgress) currentMessage(now time.Time) string {
	bytes := p.bytes.Load()
	files := p.files.Load()
	elapsed := now.Sub(p.started)

	var b strings.Builder
	if p.multi {
		done := int(p.done.Load())
		p.inFlightMu.Lock()
		inFlight := len(p.inFlight)
		// Snapshot before computing slow pod so we don't hold the lock through formatting.
		flightCopy := make(map[string]time.Time, len(p.inFlight))
		for k, v := range p.inFlight {
			flightCopy[k] = v
		}
		p.inFlightMu.Unlock()
		fmt.Fprintf(&b, "Copying to %d pods · %d/%d done · %d in flight",
			p.total, done, p.total, inFlight)

		if bytes > 0 {
			fmt.Fprintf(&b, " · %s", formatBytes(bytes))
		}
		if rate := p.rateBPS; rate > 0 {
			fmt.Fprintf(&b, " · %s", formatRate(rate))
		}
		if elapsed >= cpProgressElapsedShowFrom {
			fmt.Fprintf(&b, " · %s", formatElapsed(elapsed))
		}
		if pod, slow, ok := slowestPod(flightCopy, now); ok {
			fmt.Fprintf(&b, " · slow: %s (%s)", pod, slow.Round(time.Second))
		}
		return b.String()
	}

	b.WriteString(p.prefix)
	if files > 0 {
		fmt.Fprintf(&b, " · %d files", files)
	}
	if bytes > 0 {
		fmt.Fprintf(&b, " · %s", formatBytes(bytes))
	}
	if rate := p.rateBPS; rate > 0 {
		fmt.Fprintf(&b, " · %s", formatRate(rate))
	}
	if elapsed >= cpProgressElapsedShowFrom {
		fmt.Fprintf(&b, " · %s", formatElapsed(elapsed))
	}
	return b.String()
}

// slowestPod returns the in-flight pod whose elapsed time is at least
// cpProgressSlowFactor× the median and at least cpProgressSlowMinElapsed.
// Returns ok=false when no pod qualifies, which suppresses surfacing on
// fast jobs and on jobs where everything is moving in lockstep.
func slowestPod(inFlight map[string]time.Time, now time.Time) (string, time.Duration, bool) {
	if len(inFlight) == 0 {
		return "", 0, false
	}
	type podAge struct {
		name    string
		elapsed time.Duration
	}
	ages := make([]podAge, 0, len(inFlight))
	for name, started := range inFlight {
		ages = append(ages, podAge{name: name, elapsed: now.Sub(started)})
	}
	sort.Slice(ages, func(i, j int) bool { return ages[i].elapsed < ages[j].elapsed })
	median := ages[len(ages)/2].elapsed
	worst := ages[len(ages)-1]
	if worst.elapsed < cpProgressSlowMinElapsed {
		return "", 0, false
	}
	if float64(worst.elapsed) < cpProgressSlowFactor*float64(median) {
		return "", 0, false
	}
	return worst.name, worst.elapsed, true
}

func formatBytes(n int64) string {
	const (
		kib = int64(1024)
		mib = kib * 1024
		gib = mib * 1024
		tib = gib * 1024
	)
	switch {
	case n >= tib:
		return fmt.Sprintf("%.1f TiB", float64(n)/float64(tib))
	case n >= gib:
		return fmt.Sprintf("%.1f GiB", float64(n)/float64(gib))
	case n >= mib:
		return fmt.Sprintf("%.1f MiB", float64(n)/float64(mib))
	case n >= kib:
		return fmt.Sprintf("%.1f KiB", float64(n)/float64(kib))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func formatRate(bps int64) string {
	const (
		kib = int64(1024)
		mib = kib * 1024
		gib = mib * 1024
	)
	switch {
	case bps >= gib:
		return fmt.Sprintf("%.1f GiB/s", float64(bps)/float64(gib))
	case bps >= mib:
		return fmt.Sprintf("%.1f MiB/s", float64(bps)/float64(mib))
	case bps >= kib:
		return fmt.Sprintf("%.1f KiB/s", float64(bps)/float64(kib))
	default:
		return fmt.Sprintf("%d B/s", bps)
	}
}

func formatElapsed(d time.Duration) string {
	total := int(d.Seconds())
	return fmt.Sprintf("%02d:%02d", total/60, total%60)
}
