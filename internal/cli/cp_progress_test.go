package cli

import (
	"bytes"
	"io"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestCountingReaderAccumulatesBytes(t *testing.T) {
	src := bytes.NewReader([]byte("hello world"))
	var total int64
	cr := &countingReader{r: src, onBytes: func(n int64) { total += n }}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		t.Fatalf("copy: %v", err)
	}
	if total != int64(len("hello world")) {
		t.Fatalf("total = %d, want %d", total, len("hello world"))
	}
}

func TestCountingWriterAccumulatesBytes(t *testing.T) {
	var buf bytes.Buffer
	var total int64
	cw := &countingWriter{w: &buf, onBytes: func(n int64) { total += n }}
	if _, err := cw.Write([]byte("hello world")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if total != int64(len("hello world")) {
		t.Fatalf("total = %d, want %d", total, len("hello world"))
	}
	if buf.String() != "hello world" {
		t.Fatalf("inner writer got %q", buf.String())
	}
}

func TestCountingReaderNilCallbackSafe(t *testing.T) {
	src := bytes.NewReader([]byte("ok"))
	cr := &countingReader{r: src}
	if _, err := io.Copy(io.Discard, cr); err != nil {
		t.Fatalf("copy: %v", err)
	}
}

func TestCountingWriterNilCallbackSafe(t *testing.T) {
	var buf bytes.Buffer
	cw := &countingWriter{w: &buf}
	if _, err := cw.Write([]byte("ok")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if buf.String() != "ok" {
		t.Fatalf("inner writer got %q", buf.String())
	}
}

func TestFormatBytesRanges(t *testing.T) {
	cases := []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{512, "512 B"},
		{1024, "1.0 KiB"},
		{1500, "1.5 KiB"},
		{2 * 1024 * 1024, "2.0 MiB"},
		{int64(2.5 * float64(1024*1024*1024)), "2.5 GiB"},
		{int64(1.25 * float64(1024*1024*1024*1024)), "1.2 TiB"},
	}
	for _, c := range cases {
		if got := formatBytes(c.in); got != c.want {
			t.Errorf("formatBytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestFormatRateRanges(t *testing.T) {
	if got := formatRate(0); got != "0 B/s" {
		t.Errorf("zero rate = %q", got)
	}
	rate := 38.2 * float64(1024*1024)
	if got := formatRate(int64(rate)); got != "38.2 MiB/s" {
		t.Errorf("MiB/s rate = %q", got)
	}
}

func TestFormatElapsedMMSS(t *testing.T) {
	if got := formatElapsed(3 * time.Second); got != "00:03" {
		t.Errorf("3s = %q", got)
	}
	if got := formatElapsed(63 * time.Second); got != "01:03" {
		t.Errorf("63s = %q", got)
	}
	if got := formatElapsed(3725 * time.Second); got != "62:05" {
		t.Errorf("3725s = %q", got)
	}
}

func TestSinglePodProgressNonTTYIsNoOp(t *testing.T) {
	var buf bytes.Buffer
	p := newSinglePodProgress(&buf, "Copying ./a → :/b")
	p.start()
	p.addBytes(1024)
	p.stop()
	if buf.Len() != 0 {
		t.Fatalf("expected no output on non-tty, got %q", buf.String())
	}
}

func TestSinglePodProgressMessageIncludesBytesAndElapsed(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "Copying ./a → :/b")
	p.bytes.Store(1024 * 1024) // 1 MiB
	p.started = time.Now().Add(-5 * time.Second)
	p.lastSample = p.started
	p.lastBytes = 0
	msg := p.currentMessage(time.Now())
	if !strings.Contains(msg, "Copying ./a → :/b") {
		t.Errorf("message missing prefix: %q", msg)
	}
	if !strings.Contains(msg, "1.0 MiB") {
		t.Errorf("message missing bytes: %q", msg)
	}
	if !strings.Contains(msg, "00:05") {
		t.Errorf("message missing elapsed: %q", msg)
	}
}

func TestSinglePodProgressMessageOmitsElapsedBeforeThreshold(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "Copying ./a → :/b")
	p.bytes.Store(1024)
	p.started = time.Now().Add(-1 * time.Second) // below 3s threshold
	msg := p.currentMessage(time.Now())
	if strings.Contains(msg, "00:0") {
		t.Errorf("expected no elapsed before threshold, got %q", msg)
	}
}

func TestSinglePodProgressIncludesFilesForDirUploads(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "Copying ./repo → :/repo")
	p.addFile()
	p.addFile()
	p.addFile()
	p.bytes.Store(2048)
	p.started = time.Now().Add(-4 * time.Second)
	msg := p.currentMessage(time.Now())
	if !strings.Contains(msg, "3 files") {
		t.Errorf("message missing files count: %q", msg)
	}
}

func TestRateRollingWindow(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "x")
	now := time.Now()
	p.started = now.Add(-10 * time.Second)
	p.lastSample = now.Add(-1 * time.Second)
	p.lastBytes = 0
	p.bytes.Store(2 * 1024 * 1024) // 2 MiB in last 1s window
	p.sampleRate(now)
	rate := p.rateBPS
	expected := int64(2 * 1024 * 1024)
	// Allow 5% drift due to time.Now precision
	if rate < int64(float64(expected)*0.95) || rate > int64(float64(expected)*1.05) {
		t.Errorf("rate = %d, want ~%d", rate, expected)
	}
}

func TestMultiPodProgressMessageAggregates(t *testing.T) {
	p := newMultiPodProgress(io.Discard, 8)
	p.bytes.Store(1024 * 1024 * 1024) // 1 GiB
	p.done.Store(3)
	p.startPod("worker-0")
	p.startPod("worker-1")
	p.startPod("worker-2")
	p.startPod("worker-3")
	p.startPod("worker-4")
	p.started = time.Now().Add(-42 * time.Second)
	msg := p.currentMessage(time.Now())
	for _, want := range []string{"Copying to 8 pods", "3/8 done", "5 in flight", "1.0 GiB", "00:42"} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q: %s", want, msg)
		}
	}
}

func TestMultiPodProgressSurfacesSlowPod(t *testing.T) {
	p := newMultiPodProgress(io.Discard, 4)
	now := time.Now()
	// median elapsed = ~5s, max = 60s on worker-3 → should surface
	p.inFlight["worker-0"] = now.Add(-5 * time.Second)
	p.inFlight["worker-1"] = now.Add(-5 * time.Second)
	p.inFlight["worker-2"] = now.Add(-6 * time.Second)
	p.inFlight["worker-3"] = now.Add(-60 * time.Second)
	p.started = now.Add(-60 * time.Second)
	msg := p.currentMessage(now)
	if !strings.Contains(msg, "slow: worker-3") {
		t.Errorf("expected slow-pod surfacing, got: %s", msg)
	}
	if !strings.Contains(msg, "1m0s") && !strings.Contains(msg, "60s") {
		t.Errorf("expected slow-pod elapsed, got: %s", msg)
	}
}

func TestMultiPodProgressDoesNotSurfaceFastPods(t *testing.T) {
	p := newMultiPodProgress(io.Discard, 4)
	now := time.Now()
	// All in-flight ~5s — no surfacing
	p.inFlight["worker-0"] = now.Add(-5 * time.Second)
	p.inFlight["worker-1"] = now.Add(-6 * time.Second)
	p.started = now.Add(-6 * time.Second)
	msg := p.currentMessage(now)
	if strings.Contains(msg, "slow:") {
		t.Errorf("did not expect slow-pod surfacing, got: %s", msg)
	}
}

func TestMultiPodProgressSlowPodSuppressedUnderMinElapsed(t *testing.T) {
	p := newMultiPodProgress(io.Discard, 4)
	now := time.Now()
	// max=8s, > 2× median (4s), but below 10s minimum → suppressed
	p.inFlight["worker-0"] = now.Add(-4 * time.Second)
	p.inFlight["worker-1"] = now.Add(-4 * time.Second)
	p.inFlight["worker-2"] = now.Add(-8 * time.Second)
	p.started = now.Add(-8 * time.Second)
	msg := p.currentMessage(now)
	if strings.Contains(msg, "slow:") {
		t.Errorf("slow surfacing should suppress under min elapsed, got: %s", msg)
	}
}

func TestSinglePodProgressConcurrentAddBytesIsRaceSafe(t *testing.T) {
	p := newSinglePodProgress(io.Discard, "x")
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 1000; j++ {
				p.addBytes(1)
			}
		}()
	}
	wg.Wait()
	if got := p.bytes.Load(); got != 32*1000 {
		t.Fatalf("bytes = %d, want %d", got, 32*1000)
	}
}
