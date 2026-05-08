package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
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
	if err := extractSingleFileFromTar(bytes.NewReader(buf.Bytes()), outPath); err != nil {
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

	err := extractSingleFileFromTar(bytes.NewReader(buf.Bytes()), filepath.Join(t.TempDir(), "out.txt"))
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
	err := extractSingleFileFromTar(bytes.NewReader(truncated), filepath.Join(t.TempDir(), "out.txt"))
	if err == nil {
		t.Fatal("expected truncated tar extraction to fail")
	}
}

func TestResolveSingleFileDownloadStatePromotesUndersizedFinalFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(finalPath, bytes.Repeat([]byte("a"), 3), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := resolveSingleFileDownloadState(finalPath, 10)
	if err != nil {
		t.Fatalf("resolveSingleFileDownloadState: %v", err)
	}
	if state.ResumeOffset != 3 {
		t.Fatalf("resume offset = %d, want 3", state.ResumeOffset)
	}
	if state.PartialPath != finalPath+".okdev-part" {
		t.Fatalf("partial path = %q, want %q", state.PartialPath, finalPath+".okdev-part")
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("expected final path to be renamed away, stat err = %v", err)
	}
}

func TestResolveSingleFileDownloadStatePrefersLargerPartialSource(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	if err := os.WriteFile(finalPath, bytes.Repeat([]byte("a"), 3), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(partialPath, bytes.Repeat([]byte("b"), 7), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := resolveSingleFileDownloadState(finalPath, 10)
	if err != nil {
		t.Fatalf("resolveSingleFileDownloadState: %v", err)
	}
	if state.ResumeOffset != 7 {
		t.Fatalf("resume offset = %d, want 7", state.ResumeOffset)
	}
	if _, err := os.Stat(finalPath); !os.IsNotExist(err) {
		t.Fatalf("expected smaller final file to be removed, stat err = %v", err)
	}
}

func TestResolveSingleFileDownloadStateRejectsOversizedLocalFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(finalPath, bytes.Repeat([]byte("a"), 12), 0o644); err != nil {
		t.Fatal(err)
	}

	_, err := resolveSingleFileDownloadState(finalPath, 10)
	if err == nil || !strings.Contains(err.Error(), "larger than remote file") {
		t.Fatalf("expected oversized file error, got %v", err)
	}
}

func TestResolveSingleFileDownloadStateTreatsCompleteFinalFileAsDone(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	if err := os.WriteFile(finalPath, bytes.Repeat([]byte("a"), 10), 0o644); err != nil {
		t.Fatal(err)
	}

	state, err := resolveSingleFileDownloadState(finalPath, 10)
	if err != nil {
		t.Fatalf("resolveSingleFileDownloadState: %v", err)
	}
	if !state.AlreadyComplete {
		t.Fatal("expected already-complete state")
	}
}

func TestDownloadSingleFileResumableCompletesFromExistingPartial(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(partialPath, remoteData[:4], 0o644); err != nil {
		t.Fatal(err)
	}

	var offsets []int64
	err := downloadSingleFileResumable(context.Background(), finalPath, remoteFileInfo{
		Size: 10,
		Mode: 0o640,
	}, func(offset int64) (io.ReadCloser, error) {
		offsets = append(offsets, offset)
		return io.NopCloser(bytes.NewReader(remoteData[offset:])), nil
	}, CopyProgress{})
	if err != nil {
		t.Fatalf("downloadSingleFileResumable: %v", err)
	}
	if !reflect.DeepEqual(offsets, []int64{4}) {
		t.Fatalf("offsets = %v, want [4]", offsets)
	}
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(remoteData) {
		t.Fatalf("final data = %q, want %q", data, remoteData)
	}
	info, err := os.Stat(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o640 {
		t.Fatalf("mode = %o, want 640", info.Mode().Perm())
	}
}

func TestDownloadSingleFileResumableRetriesFromAdvancedOffset(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	remoteData := []byte("abcdefghij")

	var offsets []int64
	attempt := 0
	err := downloadSingleFileResumable(context.Background(), finalPath, remoteFileInfo{
		Size: 10,
		Mode: 0o644,
	}, func(offset int64) (io.ReadCloser, error) {
		offsets = append(offsets, offset)
		attempt++
		if attempt == 1 {
			return io.NopCloser(io.MultiReader(
				bytes.NewReader(remoteData[offset:6]),
				errReader{err: io.ErrUnexpectedEOF},
			)), nil
		}
		return io.NopCloser(bytes.NewReader(remoteData[offset:])), nil
	}, CopyProgress{})
	if err != nil {
		t.Fatalf("downloadSingleFileResumable: %v", err)
	}
	if !reflect.DeepEqual(offsets, []int64{0, 6}) {
		t.Fatalf("offsets = %v, want [0 6]", offsets)
	}
}

func TestDownloadSingleFileResumableTreatsLateRetryableErrorAsSuccess(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	remoteData := []byte("abcdefghij")

	err := downloadSingleFileResumable(context.Background(), finalPath, remoteFileInfo{
		Size: 10,
		Mode: 0o644,
	}, func(offset int64) (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(remoteData[offset:])), nil
	}, CopyProgress{}, func() error {
		return errors.New("connection reset by peer")
	})
	if err != nil {
		t.Fatalf("expected late retryable completion error to be ignored, got %v", err)
	}
}

type errReader struct {
	err error
}

func (r errReader) Read(_ []byte) (int, error) {
	return 0, r.err
}
