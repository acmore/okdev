package kube

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

func TestParseListFilesOutput(t *testing.T) {
	raw := []byte("42\tfoo.txt\n" +
		"1024\t./sub/bar.bin\n" +
		"\n" + // blank line ignored
		"not-a-number\tbad.txt\n" + // unparseable line ignored
		"-5\tneg.txt\n" + // negative size ignored
		"0\tempty.txt\n" +
		"17\t.\n") // relative root ignored

	got := parseListFilesOutput(raw)
	want := []RemoteFileEntry{
		{Path: "foo.txt", Size: 42},
		{Path: "sub/bar.bin", Size: 1024},
		{Path: "empty.txt", Size: 0},
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("parseListFilesOutput mismatch:\n got=%+v\nwant=%+v", got, want)
	}
}

func TestBucketFilesByLPTBalancesSizes(t *testing.T) {
	entries := []RemoteFileEntry{
		{Path: "huge", Size: 100},
		{Path: "large", Size: 80},
		{Path: "big", Size: 60},
		{Path: "med1", Size: 40},
		{Path: "med2", Size: 40},
		{Path: "small1", Size: 20},
		{Path: "small2", Size: 10},
	}

	buckets := bucketFilesByLPT(entries, 3)
	if len(buckets) != 3 {
		t.Fatalf("expected 3 buckets, got %d", len(buckets))
	}
	// Every input file lands in exactly one bucket.
	seen := make(map[string]int)
	var loads []int64
	for _, b := range buckets {
		var sum int64
		for _, e := range b {
			seen[e.Path]++
			sum += e.Size
		}
		loads = append(loads, sum)
	}
	if len(seen) != len(entries) {
		t.Fatalf("expected all %d files placed, got %d unique", len(entries), len(seen))
	}
	for p, n := range seen {
		if n != 1 {
			t.Fatalf("file %q placed %d times", p, n)
		}
	}
	// Max/min load ratio should stay reasonable. Total 350, three buckets.
	sort.Slice(loads, func(i, j int) bool { return loads[i] < loads[j] })
	if loads[2]-loads[0] > 40 {
		t.Fatalf("loads too unbalanced: %+v", loads)
	}
}

func TestBucketFilesByLPTMoreBucketsThanFiles(t *testing.T) {
	entries := []RemoteFileEntry{
		{Path: "a", Size: 1},
		{Path: "b", Size: 2},
	}
	buckets := bucketFilesByLPT(entries, 8)
	if len(buckets) != 2 {
		t.Fatalf("expected one bucket per file, got %d", len(buckets))
	}
}

func TestBucketFilesByLPTEmpty(t *testing.T) {
	if got := bucketFilesByLPT(nil, 4); got != nil {
		t.Fatalf("expected nil for empty input, got %+v", got)
	}
}

// TestExtractTarToDirConcurrentBuckets verifies that the file-layout invariants
// we rely on for parallel downloads hold: two tar streams covering disjoint
// files but overlapping parent directories can extract concurrently without
// collisions or errors.
func TestExtractTarToDirConcurrentBuckets(t *testing.T) {
	mkTar := func(entries map[string]string) *bytes.Reader {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		for name, content := range entries {
			if err := tw.WriteHeader(&tar.Header{
				Name:     name,
				Mode:     0o644,
				Size:     int64(len(content)),
				Typeflag: tar.TypeReg,
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
		if err := tw.Close(); err != nil {
			t.Fatal(err)
		}
		return bytes.NewReader(buf.Bytes())
	}

	dir := t.TempDir()
	bucketA := mkTar(map[string]string{
		"shared/parent/a1.txt":      "A1",
		"shared/parent/deep/a2.txt": "A2",
		"other/top.txt":             "TOP",
	})
	bucketB := mkTar(map[string]string{
		"shared/parent/b1.txt":      "B1",
		"shared/parent/deep/b2.txt": "B2",
	})

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	for _, src := range []*bytes.Reader{bucketA, bucketB} {
		go func(r *bytes.Reader) {
			defer wg.Done()
			if _, err := extractTarToDir(r, dir, nil); err != nil {
				errs <- err
			}
		}(src)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent extract failed: %v", err)
	}

	expected := map[string]string{
		"shared/parent/a1.txt":      "A1",
		"shared/parent/deep/a2.txt": "A2",
		"shared/parent/b1.txt":      "B1",
		"shared/parent/deep/b2.txt": "B2",
		"other/top.txt":             "TOP",
	}
	for rel, want := range expected {
		got, err := os.ReadFile(filepath.Join(dir, rel))
		if err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}
}

func TestListLocalFilesIgnoresNonRegular(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "sub", "g.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := listLocalFiles(dir)
	if err != nil {
		t.Fatal(err)
	}
	paths := make(map[string]int64, len(got))
	for _, e := range got {
		paths[e.Path] = e.Size
	}
	if paths["f.txt"] != 2 || paths["sub/g.txt"] != 5 {
		t.Fatalf("unexpected local enumeration: %+v", paths)
	}
	if _, hasDir := paths["sub"]; hasDir {
		t.Fatalf("directory entry should not be reported: %+v", paths)
	}
}

// TestWriteFilesTarGzipRoundTrip verifies the compression wire-up: an upload
// bucket tarred + gzipped on one side must decode cleanly with gzip.NewReader
// + extractTarToDir on the other. This is the contract each --compress
// directory copy relies on per worker.
func TestWriteFilesTarGzipRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	contents := map[string]string{
		"a.txt":           strings.Repeat("abcd", 1024),
		"sub/b.bin":       strings.Repeat("z", 2048),
		"sub/deep/c.json": "{\"ok\":true}",
	}
	entries := make([]RemoteFileEntry, 0, len(contents))
	for rel, data := range contents {
		full := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, RemoteFileEntry{Path: rel, Size: int64(len(data))})
	}

	var wire bytes.Buffer
	gz := gzip.NewWriter(&wire)
	if err := writeFilesTar(srcDir, entries, gz, nil); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}

	gr, err := gzip.NewReader(&wire)
	if err != nil {
		t.Fatal(err)
	}
	defer gr.Close()
	dstDir := t.TempDir()
	if _, err := extractTarToDir(gr, dstDir, nil); err != nil {
		t.Fatal(err)
	}
	for rel, want := range contents {
		got, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatalf("missing %s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s mismatch after gzip round-trip", rel)
		}
	}
}

// TestWriteFilesTarRoundTrip verifies writeFilesTar produces a tar stream that
// extractTarToDir can restore, which is the invariant the parallel upload path
// depends on end-to-end.
func TestWriteFilesTarRoundTrip(t *testing.T) {
	srcDir := t.TempDir()
	contents := map[string]string{
		"top.txt":           "hello",
		"sub/inner.bin":     "bytes",
		"sub/deep/end.json": "{\"a\":1}",
	}
	entries := make([]RemoteFileEntry, 0, len(contents))
	for rel, data := range contents {
		path := filepath.Join(srcDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, RemoteFileEntry{Path: rel, Size: int64(len(data))})
	}

	var buf bytes.Buffer
	if err := writeFilesTar(srcDir, entries, &buf, nil); err != nil {
		t.Fatal(err)
	}

	dstDir := t.TempDir()
	if _, err := extractTarToDir(&buf, dstDir, nil); err != nil {
		t.Fatal(err)
	}
	for rel, want := range contents {
		got, err := os.ReadFile(filepath.Join(dstDir, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s: got %q, want %q", rel, got, want)
		}
	}
}

// Sanity: parseListFilesOutput must handle the printed format we actually emit.
func TestParseListFilesOutputMatchesFindPrintfFormat(t *testing.T) {
	lines := []string{}
	for i := 0; i < 5; i++ {
		lines = append(lines, fmt.Sprintf("%d\tpath/%d.txt", 100*i, i))
	}
	raw := []byte(joinLines(lines))
	got := parseListFilesOutput(raw)
	if len(got) != 5 {
		t.Fatalf("expected 5 entries, got %d", len(got))
	}
}

func joinLines(ss []string) string {
	out := ""
	for _, s := range ss {
		out += s + "\n"
	}
	return out
}

func TestParseProbeOutputGNUStat(t *testing.T) {
	// GNU stat -c '%s %a %F' output for a 180 MB regular file.
	got, err := parseProbeOutput([]byte("188784172 644 regular file\n"))
	if err != nil {
		t.Fatal(err)
	}
	if got.Size != 188784172 {
		t.Fatalf("size: got %d", got.Size)
	}
	if got.Mode != 0o644 {
		t.Fatalf("mode: got %o", got.Mode)
	}
	if !got.IsRegular {
		t.Fatal("expected IsRegular=true")
	}
}

func TestParseProbeOutputDirectory(t *testing.T) {
	got, err := parseProbeOutput([]byte("4096 755 directory"))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsRegular {
		t.Fatal("directory must not be IsRegular")
	}
}

func TestParseProbeOutputFallbackFormat(t *testing.T) {
	// The portable fallback in probeRemoteFile emits "regular" or "other"
	// rather than "regular file".
	got, err := parseProbeOutput([]byte("100 644 regular"))
	if err != nil || !got.IsRegular {
		t.Fatalf("parse=%+v err=%v", got, err)
	}
	got, err = parseProbeOutput([]byte("100 644 other"))
	if err != nil || got.IsRegular {
		t.Fatalf("parse=%+v err=%v", got, err)
	}
}

func TestParseProbeOutputEmpty(t *testing.T) {
	if _, err := parseProbeOutput([]byte("")); err == nil {
		t.Fatal("expected error on empty output")
	}
	if _, err := parseProbeOutput([]byte("bad")); err == nil {
		t.Fatal("expected error on malformed output")
	}
}

func TestSplitFileRangesEqualSplit(t *testing.T) {
	ranges := splitFileRanges(200, 4)
	if len(ranges) != 4 {
		t.Fatalf("expected 4 ranges, got %d", len(ranges))
	}
	want := [][2]int64{{0, 50}, {50, 50}, {100, 50}, {150, 50}}
	if !reflect.DeepEqual(ranges, want) {
		t.Fatalf("got %v want %v", ranges, want)
	}
}

func TestSplitFileRangesRemainderGoesToLast(t *testing.T) {
	ranges := splitFileRanges(203, 4)
	// Base chunk 203/4 = 50, remainder (3) lands on the last worker.
	want := [][2]int64{{0, 50}, {50, 50}, {100, 50}, {150, 53}}
	if !reflect.DeepEqual(ranges, want) {
		t.Fatalf("got %v want %v", ranges, want)
	}
	// Ranges must be contiguous and cover exactly [0, 203).
	var covered int64
	for _, r := range ranges {
		if r[0] != covered {
			t.Fatalf("gap before %v", r)
		}
		covered += r[1]
	}
	if covered != 203 {
		t.Fatalf("coverage %d != 203", covered)
	}
}

func TestSplitFileRangesSmallerThanN(t *testing.T) {
	// When N > size, we get the size as a single range (the remainder
	// absorbs everything, earlier zero-length ranges are dropped).
	ranges := splitFileRanges(3, 10)
	if len(ranges) != 1 || ranges[0] != [2]int64{0, 3} {
		t.Fatalf("got %v", ranges)
	}
}

func TestSplitFileRangesZero(t *testing.T) {
	if got := splitFileRanges(0, 4); got != nil {
		t.Fatalf("expected nil for zero size, got %v", got)
	}
}

func TestAdaptiveFileParallelismRespectsCap(t *testing.T) {
	// File under threshold always drops to 1.
	if got := adaptiveFileParallelism(8*1024*1024, 8); got != 1 {
		t.Fatalf("small file: got %d, want 1", got)
	}
	// 64 MiB file is big enough for up to 4 workers (64/16 = 4), capped at
	// requested=8, so we get 4.
	if got := adaptiveFileParallelism(64*1024*1024, 8); got != 4 {
		t.Fatalf("64MiB,8: got %d, want 4", got)
	}
	// 1 GiB file ideal = 64 workers but user caps at 4.
	if got := adaptiveFileParallelism(1*1024*1024*1024, 4); got != 4 {
		t.Fatalf("1GiB,4: got %d, want 4", got)
	}
	// Invalid requested count is floored to 1.
	if got := adaptiveFileParallelism(1*1024*1024*1024, 0); got != 1 {
		t.Fatalf("requested=0: got %d, want 1", got)
	}
}

// TestOffsetWriterConcurrent verifies that concurrent writes to disjoint
// offsets through offsetWriter land correctly — this is the concurrency
// contract every parallel range worker depends on.
func TestOffsetWriterConcurrent(t *testing.T) {
	tmp, err := os.CreateTemp(t.TempDir(), "off-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()
	const total = 1024 * 1024 // 1 MiB
	if err := tmp.Truncate(total); err != nil {
		t.Fatal(err)
	}

	const workers = 8
	chunk := total / workers
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			w := &offsetWriter{dst: tmp, offset: int64(idx * chunk)}
			// Write `chunk` bytes of the worker's index so we can verify
			// ranges didn't get scrambled.
			data := bytes.Repeat([]byte{byte('A' + idx)}, chunk)
			if _, err := w.Write(data); err != nil {
				t.Errorf("worker %d: %v", idx, err)
			}
		}(i)
	}
	wg.Wait()

	buf := make([]byte, total)
	if _, err := tmp.ReadAt(buf, 0); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < workers; i++ {
		expected := byte('A' + i)
		for j := 0; j < chunk; j++ {
			if buf[i*chunk+j] != expected {
				t.Fatalf("worker %d byte %d: got %q want %q",
					i, j, buf[i*chunk+j], expected)
			}
		}
	}
}
