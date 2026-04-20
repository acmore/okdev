package cli

import (
	"bytes"
	"io"
	"strings"
	"testing"
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

// discardTTY satisfies io.Writer but pretends to expose a file descriptor so
// isInteractiveWriter would return true on interactive builds. It exists to
// ensure the renderer goroutine starts and stop() drains it cleanly without
// panics when the output is a TTY-like target.
type discardTTY struct{ io.Writer }

func TestCpProgressStopIsIdempotent(t *testing.T) {
	var buf bytes.Buffer
	p := newCpProgress(&buf, "x", 0)
	p.stop()
	p.stop()
}
