package kube

import (
	"archive/tar"
	"bytes"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateTarFromDir(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	sub := filepath.Join(dir, "sub")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(sub, "b.txt"), []byte("world"), 0o644); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := createTarFromDir(dir, &buf, nil); err != nil {
		t.Fatal(err)
	}

	// Read back and verify entries.
	tr := tar.NewReader(&buf)
	found := map[string]string{}
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if hdr.Typeflag == tar.TypeReg {
			data, _ := io.ReadAll(tr)
			found[hdr.Name] = string(data)
		}
	}
	if found["a.txt"] != "hello" {
		t.Fatalf("expected a.txt=hello, got %v", found)
	}
	if found["sub/b.txt"] != "world" {
		t.Fatalf("expected sub/b.txt=world, got %v", found)
	}
}

func TestExtractTarToDir(t *testing.T) {
	// Create a tar in memory.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	tw.WriteHeader(&tar.Header{Name: "f.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("abc"))
	tw.WriteHeader(&tar.Header{Name: "d/", Mode: 0o755, Typeflag: tar.TypeDir})
	tw.WriteHeader(&tar.Header{Name: "d/g.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg})
	tw.Write([]byte("def"))
	tw.Close()

	dir := t.TempDir()
	if _, err := extractTarToDir(&buf, dir, nil); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(filepath.Join(dir, "f.txt"))
	if err != nil || string(data) != "abc" {
		t.Fatalf("f.txt: %v %q", err, data)
	}
	data, err = os.ReadFile(filepath.Join(dir, "d", "g.txt"))
	if err != nil || string(data) != "def" {
		t.Fatalf("d/g.txt: %v %q", err, data)
	}
}

func TestExtractSingleFileFromTar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "f.txt", Mode: 0o640, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(t.TempDir(), "out.txt")
	if _, err := extractSingleFileFromTar(bytes.NewReader(buf.Bytes()), outPath, nil); err != nil {
		t.Fatal(err)
	}

	data, err := os.ReadFile(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "abc" {
		t.Fatalf("unexpected file content %q", data)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("unexpected file mode %o", info.Mode().Perm())
	}
}

func TestExtractSingleFileFromTarRejectsMultipleFiles(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for _, name := range []string{"a.txt", "b.txt"} {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o644, Size: 1, Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	_, err := extractSingleFileFromTar(bytes.NewReader(buf.Bytes()), filepath.Join(t.TempDir(), "out.txt"), nil)
	if err == nil || err.Error() != "tar archive contains multiple files" {
		t.Fatalf("expected multiple files error, got %v", err)
	}
}

func TestExtractSingleFileFromTarRejectsTruncatedArchive(t *testing.T) {
	payload := bytes.Repeat([]byte("a"), 2048)
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "f.txt", Mode: 0o644, Size: int64(len(payload)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(payload); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	truncated := buf.Bytes()[:len(buf.Bytes())-1100]
	_, err := extractSingleFileFromTar(bytes.NewReader(truncated), filepath.Join(t.TempDir(), "out.txt"), nil)
	if err == nil {
		t.Fatal("expected truncated tar extraction to fail")
	}
}

func TestExtractTarToDirReportsProduced(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "a.txt", Mode: 0o644, Size: 3, Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write([]byte("abc")); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	produced, err := extractTarToDir(bytes.NewReader(buf.Bytes()), dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !produced {
		t.Fatal("expected produced=true after writing a file")
	}
}

func TestExtractTarToDirEmptyArchiveReportsNoProduced(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "d/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dir := t.TempDir()
	produced, err := extractTarToDir(bytes.NewReader(buf.Bytes()), dir, nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if produced {
		t.Fatal("directory-only archive must report produced=false")
	}
}

func TestCreateTarFromDirInvokesProgress(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	var bytesSeen int64
	starts := []string{}
	ends := []string{}
	progress := &CopyProgress{
		OnFileStart: func(name string, _ int64) { starts = append(starts, name) },
		OnBytes:     func(n int) { bytesSeen += int64(n) },
		OnFileEnd:   func(name string) { ends = append(ends, name) },
	}

	var buf bytes.Buffer
	if err := createTarFromDir(dir, &buf, progress); err != nil {
		t.Fatal(err)
	}
	if len(starts) != 1 || starts[0] != "a.txt" {
		t.Fatalf("unexpected file starts: %v", starts)
	}
	if len(ends) != 1 || ends[0] != "a.txt" {
		t.Fatalf("unexpected file ends: %v", ends)
	}
	if bytesSeen != int64(len("hello")) {
		t.Fatalf("bytes seen = %d, want %d", bytesSeen, len("hello"))
	}
}
