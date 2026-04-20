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
	files    int64
	bytes    int64
	lastLen  int
	stopCh   chan struct{}
	doneCh   chan struct{}
	stopOnce sync.Once

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
	}
}

// kubeBytesOnly returns a *kube.CopyProgress that only reports byte counts.
// Intended for multi-pod aggregation where per-file names from concurrent
// pods would interleave meaninglessly.
func (p *cpProgress) kubeBytesOnly() *kube.CopyProgress {
	return &kube.CopyProgress{OnBytes: p.onBytes}
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
	files := p.files
	prefix := p.prefix
	p.mu.Unlock()
	bytes := atomic.LoadInt64(&p.bytes)

	elapsed := time.Since(p.started)
	rate := float64(bytes) / elapsed.Seconds()
	if elapsed.Seconds() <= 0 {
		rate = 0
	}

	var line strings.Builder
	if prefix != "" {
		line.WriteString(prefix)
		line.WriteString(" ")
	}
	line.WriteString(humanBytes(bytes))
	if p.total > 0 {
		fmt.Fprintf(&line, "/%s (%d%%)", humanBytes(p.total), pct(bytes, p.total))
	}
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
// writers.
func (p *cpProgress) summary() string {
	elapsed := time.Since(p.started)
	bytes := atomic.LoadInt64(&p.bytes)
	rate := float64(bytes) / elapsed.Seconds()
	if elapsed.Seconds() <= 0 {
		rate = 0
	}
	p.mu.Lock()
	files := p.files
	p.mu.Unlock()

	var s strings.Builder
	s.WriteString(humanBytes(bytes))
	if files > 0 {
		fmt.Fprintf(&s, " in %d files", files)
	}
	fmt.Fprintf(&s, " · %s · %s/s", elapsed.Round(10*time.Millisecond), humanBytes(int64(rate)))
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
