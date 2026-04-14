package cli

import (
	"bytes"
	"fmt"
	"strings"
	"sync"
	"testing"
)

func TestShortPodNames(t *testing.T) {
	tests := []struct {
		name  string
		pods  []string
		short []string
	}{
		{
			name:  "common prefix stripped at dash boundary",
			pods:  []string{"okdev-sess-worker-0", "okdev-sess-worker-1"},
			short: []string{"worker-0", "worker-1"},
		},
		{
			name:  "single pod keeps full name",
			pods:  []string{"okdev-sess"},
			short: []string{"okdev-sess"},
		},
		{
			name:  "no common prefix",
			pods:  []string{"alpha-0", "beta-1"},
			short: []string{"alpha-0", "beta-1"},
		},
		{
			name:  "identical names stay as-is",
			pods:  []string{"pod-a", "pod-a"},
			short: []string{"pod-a", "pod-a"},
		},
		{
			name:  "empty input",
			pods:  []string{},
			short: []string{},
		},
		{
			name:  "keeps at least two segments",
			pods:  []string{"okdev-sess-0", "okdev-sess-1"},
			short: []string{"sess-0", "sess-1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := shortPodNames(tt.pods)
			if len(got) != len(tt.short) {
				t.Fatalf("expected %d names, got %d: %v", len(tt.short), len(got), got)
			}
			for i := range got {
				if got[i] != tt.short[i] {
					t.Fatalf("index %d: expected %q, got %q", i, tt.short[i], got[i])
				}
			}
		})
	}
}

func TestMaxPrefixWidth(t *testing.T) {
	if got := maxPrefixWidth([]string{"ab", "abcde", "abc"}); got != 5 {
		t.Fatalf("expected 5, got %d", got)
	}
	if got := maxPrefixWidth(nil); got != 0 {
		t.Fatalf("expected 0 for nil, got %d", got)
	}
}

func TestFormatPodPrefixesNoColor(t *testing.T) {
	names := []string{"worker-0", "worker-10", "master-0"}
	got := formatPodPrefixes(names, false)
	// Brackets + padded to 9 chars (len("worker-10")) + brackets = 11.
	if got[0] != "[worker-0 ]" || got[1] != "[worker-10]" || got[2] != "[master-0 ]" {
		t.Fatalf("unexpected prefixes: %v", got)
	}
}

func TestFormatPodPrefixesWithColor(t *testing.T) {
	names := []string{"a", "bb"}
	got := formatPodPrefixes(names, true)
	// Each should contain ANSI color code and reset.
	for i, p := range got {
		if !strings.Contains(p, "\033[") || !strings.Contains(p, prefixReset) {
			t.Fatalf("index %d: expected color codes in %q", i, p)
		}
	}
	// Different pods should get different colors.
	if got[0] == got[1] {
		t.Fatalf("expected different colors for different pods")
	}
}

func TestPrefixedWriterCompleteLine(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newPrefixedWriter("w0", &buf, &mu)
	fmt.Fprint(w, "hello world\n")
	w.Flush()
	if got := buf.String(); got != "w0 hello world\n" {
		t.Fatalf("expected %q, got %q", "w0 hello world\n", got)
	}
}

func TestPrefixedWriterPartialLines(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newPrefixedWriter("w0", &buf, &mu)
	fmt.Fprint(w, "hel")
	if buf.Len() != 0 {
		t.Fatalf("expected no output before newline, got %q", buf.String())
	}
	fmt.Fprint(w, "lo\n")
	if got := buf.String(); got != "w0 hello\n" {
		t.Fatalf("expected %q, got %q", "w0 hello\n", got)
	}
}

func TestPrefixedWriterMultipleLines(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newPrefixedWriter("n1", &buf, &mu)
	fmt.Fprint(w, "line1\nline2\n")
	if got := buf.String(); got != "n1 line1\nn1 line2\n" {
		t.Fatalf("expected prefixed lines, got %q", got)
	}
}

func TestPrefixedWriterFlushPartial(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	w := newPrefixedWriter("p", &buf, &mu)
	fmt.Fprint(w, "no newline")
	w.Flush()
	if got := buf.String(); got != "p no newline\n" {
		t.Fatalf("expected flushed partial line, got %q", got)
	}
}

func TestPrefixedWriterConcurrent(t *testing.T) {
	var buf bytes.Buffer
	var mu sync.Mutex
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			w := newPrefixedWriter(fmt.Sprintf("g%d", id), &buf, &mu)
			for j := 0; j < 20; j++ {
				fmt.Fprintf(w, "line %d\n", j)
			}
			w.Flush()
		}(i)
	}
	wg.Wait()
	lines := bytes.Count(buf.Bytes(), []byte("\n"))
	if lines != 200 {
		t.Fatalf("expected 200 lines, got %d", lines)
	}
}
