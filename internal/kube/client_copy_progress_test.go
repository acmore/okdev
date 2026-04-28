package kube

import (
	"bytes"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
)

func TestCreateTarFromDirInvokesOnFileForEachFile(t *testing.T) {
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
	if err := os.WriteFile(filepath.Join(sub, "c.txt"), []byte("more"), 0o644); err != nil {
		t.Fatal(err)
	}

	var files atomic.Int64
	var buf bytes.Buffer
	if err := createTarFromDirWithProgress(dir, &buf, func() { files.Add(1) }); err != nil {
		t.Fatal(err)
	}
	if got := files.Load(); got != 3 {
		t.Fatalf("expected 3 onFile invocations, got %d", got)
	}
}

func TestCreateTarFromDirNilOnFileSafe(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := createTarFromDirWithProgress(dir, &buf, nil); err != nil {
		t.Fatalf("nil onFile should be a no-op, got %v", err)
	}
	if buf.Len() == 0 {
		t.Fatal("expected tar output even with nil onFile")
	}
}

func TestProgressByteCounterReportsAllBytesWritten(t *testing.T) {
	var total atomic.Int64
	w := byteCounterWriter(func(n int64) { total.Add(n) })
	n, err := w.Write([]byte("hello"))
	if err != nil || n != 5 {
		t.Fatalf("write returned (%d, %v)", n, err)
	}
	n, err = w.Write([]byte(" world"))
	if err != nil || n != 6 {
		t.Fatalf("write returned (%d, %v)", n, err)
	}
	if got := total.Load(); got != 11 {
		t.Fatalf("byte counter total = %d, want 11", got)
	}
}

func TestProgressByteCounterNilFuncSafe(t *testing.T) {
	w := byteCounterWriter(nil)
	if _, err := w.Write([]byte("ok")); err != nil {
		t.Fatalf("nil func should be safe, got %v", err)
	}
}
