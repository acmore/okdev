package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestHumanBytes(t *testing.T) {
	tests := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KB"},
		{1536, "1.5 KB"},
		{1 << 20, "1.00 MB"},
		{1 << 30, "1.00 GB"},
		{1 << 40, "1.00 TB"},
	}
	for _, tc := range tests {
		if got := humanBytes(tc.in); got != tc.want {
			t.Errorf("humanBytes(%d) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestPct(t *testing.T) {
	cases := []struct {
		a, b int64
		want int
	}{
		{0, 0, 0},
		{10, 0, 0},
		{50, 100, 50},
		{200, 100, 100},
		{-5, 100, 0},
	}
	for _, c := range cases {
		if got := pct(c.a, c.b); got != c.want {
			t.Errorf("pct(%d,%d) = %d, want %d", c.a, c.b, got, c.want)
		}
	}
}

func TestTruncateRight(t *testing.T) {
	if got := truncateRight("short", 10); got != "short" {
		t.Errorf("no truncation: %q", got)
	}
	got := truncateRight("/a/very/long/path/to/file.bin", 10)
	if !strings.HasPrefix(got, "…") || len(got) > 12 { // rune-aware len is bytes here
		t.Errorf("unexpected truncation: %q", got)
	}
}

func TestCpProgressNonInteractiveCaptures(t *testing.T) {
	var buf bytes.Buffer
	p := newCpProgress(&buf, "test", 100)
	// Non-TTY writer must not spawn the render goroutine or emit partial lines.
	p.onBytes(42)
	p.onFileStart("f.txt", 42)
	p.stop()
	if buf.Len() != 0 {
		t.Fatalf("expected no interim output on non-tty, got %q", buf.String())
	}
	summary := p.summary()
	if !strings.Contains(summary, "42 B") || !strings.Contains(summary, "in 1 files") {
		t.Fatalf("summary missing expected content: %q", summary)
	}
}

func TestCpProgressStopIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	p := newCpProgress(&buf, "x", 0)
	p.stop()
	p.stop()
}

func TestFormatDuration(t *testing.T) {
	cases := map[time.Duration]string{
		0:                              "1s",
		500 * time.Millisecond:         "1s",
		42 * time.Second:               "42s",
		3*time.Minute + 12*time.Second: "3m12s",
		2*time.Hour + 5*time.Minute + 30*time.Second: "2h05m",
	}
	for d, want := range cases {
		if got := formatDuration(d); got != want {
			t.Errorf("formatDuration(%s) = %q, want %q", d, got, want)
		}
	}
}

func TestEtaStringReturnsEmptyWithoutTotal(t *testing.T) {
	p := &cpProgress{total: 0}
	if got := p.etaString(100, 10); got != "" {
		t.Errorf("expected empty ETA without total, got %q", got)
	}
}

func TestEtaStringComputesRemaining(t *testing.T) {
	p := &cpProgress{total: 1000}
	got := p.etaString(200, 100) // 800 bytes remaining at 100 B/s -> 8s
	if got != "8s" {
		t.Errorf("expected ETA=8s, got %q", got)
	}
}

func TestEtaStringClampsAbsurdEstimates(t *testing.T) {
	p := &cpProgress{total: 1 << 40}
	if got := p.etaString(0, 1); got != "" {
		t.Errorf("absurd ETA should be suppressed, got %q", got)
	}
}

// TestRenderShowsPhaseBeforeBytes verifies the status line during enumeration
// presents the phase + elapsed so users don't mistake a long `find` for a
// hang. The render method only writes when interactive, so we construct a
// tracker that targets a bytes.Buffer and flip the interactive flag directly.
func TestRenderShowsPhaseBeforeBytes(t *testing.T) {
	var buf bytes.Buffer
	p := &cpProgress{w: &buf, interactive: true, prefix: "← :/data", started: time.Now(), phase: "listing remote files"}
	p.render()
	out := buf.String()
	if !strings.Contains(out, "listing remote files") {
		t.Fatalf("expected phase in rendered line, got %q", out)
	}
	if !strings.Contains(out, "1s") && !strings.Contains(out, "2s") {
		t.Fatalf("expected elapsed duration in line, got %q", out)
	}
	// Once bytes flow, the phase view must step aside for the byte progress.
	p.onBytes(512)
	buf.Reset()
	p.render()
	out = buf.String()
	if strings.Contains(out, "listing remote files") {
		t.Fatalf("phase should be hidden once bytes arrive, got %q", out)
	}
	if !strings.Contains(out, "512 B") {
		t.Fatalf("expected byte count in line, got %q", out)
	}
}

// TestCpProgressKubeWiresPhase ensures the kube CopyProgress returned by
// kube() and kubeBytesOnly() carries the phase setter so the render can react
// to long enumerations. This is the regression guard for the "frozen looking
// progress line" bug.
func TestCpProgressKubeWiresPhase(t *testing.T) {
	var buf bytes.Buffer
	p := newCpProgress(&buf, "x", 0)
	defer p.stop()

	k := p.kube()
	if k.OnPhase == nil {
		t.Fatal("kube().OnPhase must be wired")
	}
	k.OnPhase("listing remote files")
	p.mu.Lock()
	got := p.phase
	p.mu.Unlock()
	if got != "listing remote files" {
		t.Fatalf("phase not propagated, got %q", got)
	}

	kb := p.kubeBytesOnly()
	if kb.OnPhase == nil {
		t.Fatal("kubeBytesOnly().OnPhase must be wired too")
	}
	kb.OnPhase("")
	p.mu.Lock()
	got = p.phase
	p.mu.Unlock()
	if got != "" {
		t.Fatalf("phase not cleared, got %q", got)
	}
}
