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
	if err := createTarFromDir(dir, &buf); err != nil {
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
	if err := extractTarToDir(&buf, dir); err != nil {
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
