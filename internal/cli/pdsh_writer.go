package cli

import (
	"bytes"
	"fmt"
	"io"
	"strings"
	"sync"
)

// shortPodNames strips the longest common dash-delimited prefix from the
// given pod names. It splits each name by "-" and removes leading segments
// that are identical across all names, keeping at least two segments per
// name for readability.
func shortPodNames(names []string) []string {
	if len(names) <= 1 {
		return append([]string(nil), names...)
	}
	split := make([][]string, len(names))
	minSegments := len(strings.Split(names[0], "-"))
	for i, name := range names {
		split[i] = strings.Split(name, "-")
		if len(split[i]) < minSegments {
			minSegments = len(split[i])
		}
	}
	commonCount := 0
	for seg := 0; seg < minSegments; seg++ {
		val := split[0][seg]
		allSame := true
		for _, parts := range split[1:] {
			if parts[seg] != val {
				allSame = false
				break
			}
		}
		if !allSame {
			break
		}
		commonCount++
	}
	// Keep at least two segments per name for readability.
	maxStrip := minSegments - 2
	if maxStrip < 0 {
		maxStrip = 0
	}
	if commonCount > maxStrip {
		commonCount = maxStrip
	}
	if commonCount == 0 {
		return append([]string(nil), names...)
	}
	out := make([]string, len(names))
	for i, parts := range split {
		out[i] = strings.Join(parts[commonCount:], "-")
	}
	return out
}

// prefixedWriter is a thread-safe io.Writer that buffers input and emits
// complete lines prefixed with the pod short name. Partial lines are held
// until a newline arrives or Flush is called.
type prefixedWriter struct {
	prefix string
	dest   io.Writer
	mu     *sync.Mutex
	buf    bytes.Buffer
}

func newPrefixedWriter(prefix string, dest io.Writer, mu *sync.Mutex) *prefixedWriter {
	return &prefixedWriter{prefix: prefix, dest: dest, mu: mu}
}

func (w *prefixedWriter) Write(p []byte) (int, error) {
	w.buf.Write(p)
	for {
		line, err := w.buf.ReadString('\n')
		if err != nil {
			// incomplete line — put it back
			w.buf.WriteString(line)
			break
		}
		w.mu.Lock()
		fmt.Fprintf(w.dest, "%s: %s", w.prefix, line)
		w.mu.Unlock()
	}
	return len(p), nil
}

// Flush emits any remaining buffered content as a final line.
func (w *prefixedWriter) Flush() {
	if w.buf.Len() == 0 {
		return
	}
	w.mu.Lock()
	fmt.Fprintf(w.dest, "%s: %s\n", w.prefix, w.buf.String())
	w.mu.Unlock()
	w.buf.Reset()
}
