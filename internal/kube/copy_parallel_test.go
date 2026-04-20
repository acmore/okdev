package kube

import (
	"archive/tar"
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
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
		"shared/parent/a1.txt":       "A1",
		"shared/parent/deep/a2.txt":  "A2",
		"other/top.txt":              "TOP",
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
