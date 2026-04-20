package cli

import (
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

// cpProgress renders a throttled single-line status describing an in-flight
// copy. It updates in place on TTYs and falls back to a final summary on
// non-TTY writers. It is safe to use concurrently.
type cpProgress struct {
	w           io.Writer
	interactive bool
	prefix      string
	started     time.Time

	mu       sync.Mutex
	current  string
	phase    string
	files    int64
	bytes    int64
	lastLen  int
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

	// firstByteNanos records the unix-nanos instant the first byte flowed,
	// lazily set on the first OnBytes call. The transfer rate is measured
	// from this moment (rather than from progress creation) so pre-transfer
	// round-trips — IsRemoteDir, du -sb, tar process startup, remote `find`
	// for the parallel path — don't get amortized into the displayed rate.
	firstByteNanos int64

	// total is the pre-computed expected byte count (0 if unknown).
	total int64
}

const cpProgressInterval = 150 * time.Millisecond

// newCpProgress builds a progress tracker that writes to w. When w is not a
// TTY (or colors are disabled) the tracker quietly accumulates counters and
// only renders the final summary.
func newCpProgress(w io.Writer, prefix string, total int64) *cpProgress {
	p := &cpProgress{
		w:           w,
		interactive: isInteractiveWriter(w),
		prefix:      strings.TrimSpace(prefix),
		started:     time.Now(),
		total:       total,
	}
	if p.interactive {
		p.stopCh = make(chan struct{})
		p.doneCh = make(chan struct{})
		// Render once immediately so the user sees the prefix + planned total
		// without waiting for the first tick.
		p.render()
		go p.run()
	}
	return p
}

// kube returns a *kube.CopyProgress hooked into this renderer.
func (p *cpProgress) kube() *kube.CopyProgress {
	return &kube.CopyProgress{
		OnFileStart: p.onFileStart,
		OnBytes:     p.onBytes,
		OnPhase:     p.setPhase,
	}
}

// kubeBytesOnly returns a *kube.CopyProgress that only reports byte counts.
// Intended for multi-pod aggregation where per-file names from concurrent
// pods would interleave meaninglessly.
func (p *cpProgress) kubeBytesOnly() *kube.CopyProgress {
	return &kube.CopyProgress{OnBytes: p.onBytes, OnPhase: p.setPhase}
}

// setPhase updates the human-readable phase shown when no bytes have flowed
// yet (e.g. "listing remote files"). An empty string clears the phase.
func (p *cpProgress) setPhase(phase string) {
	p.mu.Lock()
	p.phase = phase
	p.mu.Unlock()
	// Render once so the transition is visible without waiting for the next
	// tick; the ticker may be ~150ms away and users perceive long enumerations
	// as hangs otherwise.
	if p.interactive {
		p.render()
	}
}

// setPrefix updates the leading prefix used on the status line.
func (p *cpProgress) setPrefix(s string) {
	p.mu.Lock()
	p.prefix = strings.TrimSpace(s)
	p.mu.Unlock()
}

func (p *cpProgress) onFileStart(name string, _ int64) {
	p.mu.Lock()
	p.current = name
	p.files++
	p.mu.Unlock()
}

func (p *cpProgress) onBytes(n int) {
	atomic.AddInt64(&p.bytes, int64(n))
	// Record the first-byte moment exactly once so the rate calculation
	// excludes pre-transfer latency. A CAS keeps this race-free across the N
	// parallel worker goroutines that may all be writing bytes simultaneously.
	if atomic.LoadInt64(&p.firstByteNanos) == 0 {
		atomic.CompareAndSwapInt64(&p.firstByteNanos, 0, time.Now().UnixNano())
	}
}

// transferElapsed returns the wall time spent actually moving bytes, i.e.
// measured from the first OnBytes callback. Before any bytes have arrived it
// returns 0 so callers can treat that case as "no rate yet".
func (p *cpProgress) transferElapsed() time.Duration {
	start := atomic.LoadInt64(&p.firstByteNanos)
	if start == 0 {
		return 0
	}
	return time.Since(time.Unix(0, start))
}

func (p *cpProgress) run() {
	defer close(p.doneCh)
	ticker := time.NewTicker(cpProgressInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			p.render()
		case <-p.stopCh:
			return
		}
	}
}

func (p *cpProgress) render() {
	p.mu.Lock()
	current := p.current
	phase := p.phase
	files := p.files
	prefix := p.prefix
	p.mu.Unlock()
	bytes := atomic.LoadInt64(&p.bytes)

	elapsed := time.Since(p.started)
	// Rate uses the transfer-only clock (time since first byte) so slow
	// pre-transfer setup doesn't drag the displayed throughput down.
	rate := 0.0
	if tx := p.transferElapsed(); tx > 0 {
		rate = float64(bytes) / tx.Seconds()
	}

	var line strings.Builder
	if prefix != "" {
		line.WriteString(prefix)
		line.WriteString(" ")
	}
	// While no bytes have flowed and we have a human-readable phase, show the
	// phase with elapsed time so long enumerations don't look like a hang. As
	// soon as the first byte lands, the normal progress rendering takes over.
	if bytes == 0 && phase != "" {
		fmt.Fprintf(&line, "%s · %s", phase, formatDuration(elapsed))
	} else {
		line.WriteString(humanBytes(bytes))
		if p.total > 0 {
			fmt.Fprintf(&line, "/%s (%d%%)", humanBytes(p.total), pct(bytes, p.total))
		}
		fmt.Fprintf(&line, " · %s", formatDuration(elapsed))
		fmt.Fprintf(&line, " · %s/s", humanBytes(int64(rate)))
		if eta := p.etaString(bytes, rate); eta != "" {
			fmt.Fprintf(&line, " · ETA %s", eta)
		}
		if files > 0 {
			fmt.Fprintf(&line, " · %d files", files)
		}
		if current != "" {
			fmt.Fprintf(&line, " · %s", truncateRight(current, 40))
		}
	}

	rendered := line.String()
	width := terminalWidth()
	if width > 0 && len(rendered) > width-2 {
		rendered = rendered[:width-2]
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	pad := ""
	if p.lastLen > len(rendered) {
		pad = strings.Repeat(" ", p.lastLen-len(rendered))
	}
	fmt.Fprintf(p.w, "\r%s%s", rendered, pad)
	p.lastLen = len(rendered)
}

// etaString returns a short human ETA (e.g. "42s", "3m12s") if a total and a
// non-trivial rate are known. It returns "" when a meaningful estimate isn't
// available yet so the status line skips the field entirely.
func (p *cpProgress) etaString(bytes int64, rate float64) string {
	if p.total <= 0 || rate <= 0 || bytes >= p.total {
		return ""
	}
	remaining := p.total - bytes
	seconds := float64(remaining) / rate
	if seconds < 0 || seconds > 60*60*24 { // clamp absurd estimates
		return ""
	}
	return formatDuration(time.Duration(seconds * float64(time.Second)))
}

// formatDuration renders a short human duration like "42s", "3m12s", "1h02m".
func formatDuration(d time.Duration) string {
	if d < time.Second {
		d = time.Second
	}
	d = d.Round(time.Second)
	h := int(d / time.Hour)
	m := int(d%time.Hour) / int(time.Minute)
	s := int(d%time.Minute) / int(time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh%02dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm%02ds", m, s)
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// summary renders a terse one-line summary of what was transferred. It is
// suitable for printing after stop() on both interactive and non-interactive
// writers. Total elapsed counts wall time since the command started; rate
// counts only the transfer window so pre-transfer setup doesn't skew it.
func (p *cpProgress) summary() string {
	totalElapsed := time.Since(p.started)
	tx := p.transferElapsed()
	bytes := atomic.LoadInt64(&p.bytes)
	rate := 0.0
	if tx > 0 {
		rate = float64(bytes) / tx.Seconds()
	}
	p.mu.Lock()
	files := p.files
	p.mu.Unlock()

	var s strings.Builder
	s.WriteString(humanBytes(bytes))
	if files > 0 {
		fmt.Fprintf(&s, " in %d files", files)
	}
	fmt.Fprintf(&s, " · %s · %s/s", totalElapsed.Round(10*time.Millisecond), humanBytes(int64(rate)))
	return s.String()
}

func (p *cpProgress) stop() {
	p.stopOnce.Do(func() {
		if p.interactive {
			close(p.stopCh)
			<-p.doneCh
			p.mu.Lock()
			if p.lastLen > 0 {
				fmt.Fprint(p.w, "\r"+strings.Repeat(" ", p.lastLen)+"\r")
				p.lastLen = 0
			}
			p.mu.Unlock()
		}
	})
}

func humanBytes(n int64) string {
	const (
		kb = 1 << 10
		mb = 1 << 20
		gb = 1 << 30
		tb = 1 << 40
	)
	switch {
	case n >= tb:
		return fmt.Sprintf("%.2f TB", float64(n)/float64(tb))
	case n >= gb:
		return fmt.Sprintf("%.2f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.2f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func pct(a, b int64) int {
	if b <= 0 {
		return 0
	}
	v := int(a * 100 / b)
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

func truncateRight(s string, max int) string {
	if max <= 0 || len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[len(s)-max:]
	}
	return "…" + s[len(s)-(max-1):]
}
