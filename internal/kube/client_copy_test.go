package kube

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
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

func TestDownloadSingleFileResumablePromotesCompletePartialWithoutRedownload(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(partialPath, remoteData, 0o640); err != nil {
		t.Fatal(err)
	}

	openCalls := 0
	err := downloadSingleFileResumable(context.Background(), finalPath, remoteFileInfo{
		Size: int64(len(remoteData)),
		Mode: 0o640,
	}, func(offset int64) (io.ReadCloser, error) {
		openCalls++
		return io.NopCloser(bytes.NewReader(remoteData[offset:])), nil
	}, CopyProgress{})
	if err != nil {
		t.Fatalf("downloadSingleFileResumable: %v", err)
	}
	if openCalls != 0 {
		t.Fatalf("expected no remote reads, got %d", openCalls)
	}
	if _, err := os.Stat(partialPath); !os.IsNotExist(err) {
		t.Fatalf("expected partial path to be promoted away, stat err = %v", err)
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

func TestBuildRemoteSingleFileRangeCommandUsesBlockSizedReads(t *testing.T) {
	// Regression: `dd skip=N bs=1` reads/writes the entire file one byte
	// at a time, turning a 1 GB transfer into ~1e9 syscalls. The remote
	// range command must use a block-sized streaming primitive.
	cases := []struct {
		offset      int64
		want        string
		mustNotHave string
	}{
		{offset: 0, want: "cat ", mustNotHave: "bs=1"},
		{offset: 100, want: "tail -c +101 ", mustNotHave: "bs=1"},
		{offset: 1 << 30, want: fmt.Sprintf("tail -c +%d ", (1<<30)+1), mustNotHave: "bs=1"},
	}
	for _, tc := range cases {
		got := buildRemoteSingleFileRangeCommand("/workspace/model.bin", tc.offset)
		if !strings.HasPrefix(got, tc.want) {
			t.Fatalf("offset=%d: expected prefix %q, got %q", tc.offset, tc.want, got)
		}
		if strings.Contains(got, tc.mustNotHave) {
			t.Fatalf("offset=%d: byte-by-byte primitive must not appear, got %q", tc.offset, got)
		}
	}
}

func TestParseRemoteSingleFileProbeOutput(t *testing.T) {
	info, err := parseRemoteSingleFileProbeOutput("10\n640\n")
	if err != nil {
		t.Fatalf("parseRemoteSingleFileProbeOutput: %v", err)
	}
	if info.Size != 10 || info.Mode != 0o640 {
		t.Fatalf("unexpected info %#v", info)
	}
}

func TestParseRemoteSingleFileProbeOutputFallsBackToDefaultMode(t *testing.T) {
	info, err := parseRemoteSingleFileProbeOutput("10\n\n")
	if err != nil {
		t.Fatalf("parseRemoteSingleFileProbeOutput: %v", err)
	}
	if info.Mode != 0o644 {
		t.Fatalf("mode = %o, want 0644", info.Mode)
	}
}

func TestCopyFromPodInContainerWithProgressUsesResumableDownloadPath(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(partialPath, remoteData[:4], 0o644); err != nil {
		t.Fatal(err)
	}

	prevProbe := probeRemoteSingleFileForCopy
	prevRange := openRemoteSingleFileRangeForCopy
	t.Cleanup(func() {
		probeRemoteSingleFileForCopy = prevProbe
		openRemoteSingleFileRangeForCopy = prevRange
	})

	var offsets []int64
	probeRemoteSingleFileForCopy = func(context.Context, *Client, string, string, string, string) (remoteFileInfo, error) {
		return remoteFileInfo{Size: 10, Mode: 0o640}, nil
	}
	openRemoteSingleFileRangeForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, _ string, offset int64) (io.ReadCloser, completionCheck, error) {
		offsets = append(offsets, offset)
		return io.NopCloser(bytes.NewReader(remoteData[offset:])), nil, nil
	}

	client := &Client{}
	if err := client.CopyFromPodInContainerWithProgress(context.Background(), "default", "pod-0", "dev", "/workspace/model.bin", finalPath, CopyProgress{}); err != nil {
		t.Fatalf("CopyFromPodInContainerWithProgress: %v", err)
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
}

func TestCopyFromPodInContainerVerifiedWithProgressResumesAndChecksChecksum(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(partialPath, remoteData[:4], 0o644); err != nil {
		t.Fatal(err)
	}

	prevProbe := probeRemoteSingleFileForCopy
	prevVerified := openRemoteVerifiedSingleFileRangeForCopy
	t.Cleanup(func() {
		probeRemoteSingleFileForCopy = prevProbe
		openRemoteVerifiedSingleFileRangeForCopy = prevVerified
	})

	wantSum := fmt.Sprintf("%x", sha256.Sum256(remoteData))
	var offsets []int64
	var gotLocalSum string
	probeRemoteSingleFileForCopy = func(context.Context, *Client, string, string, string, string) (remoteFileInfo, error) {
		return remoteFileInfo{Size: int64(len(remoteData)), Mode: 0o640}, nil
	}
	openRemoteVerifiedSingleFileRangeForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, _ string, offset int64, localSHA256 func() string) (io.ReadCloser, error) {
		offsets = append(offsets, offset)
		return &remoteRangeReadCloser{
			ReadCloser: io.NopCloser(bytes.NewReader(remoteData[offset:])),
			done: func() error {
				gotLocalSum = localSHA256()
				if gotLocalSum != wantSum {
					return fmt.Errorf("%w: remote %s local %s", errChecksumMismatch, wantSum, gotLocalSum)
				}
				return nil
			},
		}, nil
	}

	client := &Client{}
	if err := client.CopyFromPodInContainerVerifiedWithProgress(context.Background(), "default", "pod-0", "dev", "/workspace/model.bin", finalPath, CopyProgress{}); err != nil {
		t.Fatalf("CopyFromPodInContainerVerifiedWithProgress: %v", err)
	}
	if !reflect.DeepEqual(offsets, []int64{4}) {
		t.Fatalf("offsets = %v, want [4]", offsets)
	}
	if gotLocalSum != wantSum {
		t.Fatalf("local checksum = %q, want %q", gotLocalSum, wantSum)
	}
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(remoteData) {
		t.Fatalf("final data = %q, want %q", data, remoteData)
	}
}

func TestCopyFromPodInContainerVerifiedWithProgressRemovesCorruptCompletePartial(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	partialPath := finalPath + ".okdev-part"
	remoteData := []byte("abcdefghij")

	prevProbe := probeRemoteSingleFileForCopy
	prevVerified := openRemoteVerifiedSingleFileRangeForCopy
	t.Cleanup(func() {
		probeRemoteSingleFileForCopy = prevProbe
		openRemoteVerifiedSingleFileRangeForCopy = prevVerified
	})

	probeRemoteSingleFileForCopy = func(context.Context, *Client, string, string, string, string) (remoteFileInfo, error) {
		return remoteFileInfo{Size: int64(len(remoteData)), Mode: 0o640}, nil
	}
	openRemoteVerifiedSingleFileRangeForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, _ string, offset int64, localSHA256 func() string) (io.ReadCloser, error) {
		return &remoteRangeReadCloser{
			ReadCloser: io.NopCloser(bytes.NewReader(remoteData[offset:])),
			done: func() error {
				return fmt.Errorf("%w: remote %s local %s", errChecksumMismatch, strings.Repeat("0", 64), localSHA256())
			},
		}, nil
	}

	client := &Client{}
	err := client.CopyFromPodInContainerVerifiedWithProgress(context.Background(), "default", "pod-0", "dev", "/workspace/model.bin", finalPath, CopyProgress{})
	if !errors.Is(err, errChecksumMismatch) {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
	if _, statErr := os.Stat(partialPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected corrupt partial to be removed, stat err = %v", statErr)
	}
	if _, statErr := os.Stat(finalPath); !os.IsNotExist(statErr) {
		t.Fatalf("expected no final file after mismatch, stat err = %v", statErr)
	}
}

func TestCopyFromPodInContainerVerifiedWithProgressChecksAlreadyCompleteFile(t *testing.T) {
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(finalPath, remoteData, 0o640); err != nil {
		t.Fatal(err)
	}

	prevProbe := probeRemoteSingleFileForCopy
	prevSHA := computeRemoteSingleFileSHA256ForCopy
	prevVerified := openRemoteVerifiedSingleFileRangeForCopy
	t.Cleanup(func() {
		probeRemoteSingleFileForCopy = prevProbe
		computeRemoteSingleFileSHA256ForCopy = prevSHA
		openRemoteVerifiedSingleFileRangeForCopy = prevVerified
	})

	wantSum := fmt.Sprintf("%x", sha256.Sum256(remoteData))
	probeRemoteSingleFileForCopy = func(context.Context, *Client, string, string, string, string) (remoteFileInfo, error) {
		return remoteFileInfo{Size: int64(len(remoteData)), Mode: 0o640}, nil
	}
	shaCalls := 0
	computeRemoteSingleFileSHA256ForCopy = func(context.Context, *Client, string, string, string, string) (string, error) {
		shaCalls++
		return wantSum, nil
	}
	// AlreadyComplete must NOT fall through to the streaming verified
	// range — that would re-read the whole remote file for nothing.
	openRemoteVerifiedSingleFileRangeForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, _ string, offset int64, localSHA256 func() string) (io.ReadCloser, error) {
		t.Fatalf("streaming verified range must not be used when local file is already complete (offset=%d)", offset)
		return nil, nil
	}

	client := &Client{}
	if err := client.CopyFromPodInContainerVerifiedWithProgress(context.Background(), "default", "pod-0", "dev", "/workspace/model.bin", finalPath, CopyProgress{}); err != nil {
		t.Fatalf("CopyFromPodInContainerVerifiedWithProgress: %v", err)
	}
	if shaCalls != 1 {
		t.Fatalf("expected one hash-only remote call, got %d", shaCalls)
	}
	data, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(remoteData) {
		t.Fatalf("final data = %q, want %q", data, remoteData)
	}
}

func TestCopyFromPodInContainerVerifiedWithProgressFailsWhenAlreadyCompleteHashDiffers(t *testing.T) {
	// If a previous local copy is complete but the remote file has since
	// changed without a size change, the hash-only path must surface the
	// mismatch instead of silently accepting the stale local bytes.
	dir := t.TempDir()
	finalPath := filepath.Join(dir, "model.bin")
	remoteData := []byte("abcdefghij")
	if err := os.WriteFile(finalPath, remoteData, 0o640); err != nil {
		t.Fatal(err)
	}

	prevProbe := probeRemoteSingleFileForCopy
	prevSHA := computeRemoteSingleFileSHA256ForCopy
	t.Cleanup(func() {
		probeRemoteSingleFileForCopy = prevProbe
		computeRemoteSingleFileSHA256ForCopy = prevSHA
	})

	probeRemoteSingleFileForCopy = func(context.Context, *Client, string, string, string, string) (remoteFileInfo, error) {
		return remoteFileInfo{Size: int64(len(remoteData)), Mode: 0o640}, nil
	}
	computeRemoteSingleFileSHA256ForCopy = func(context.Context, *Client, string, string, string, string) (string, error) {
		return strings.Repeat("0", 64), nil
	}

	client := &Client{}
	err := client.CopyFromPodInContainerVerifiedWithProgress(context.Background(), "default", "pod-0", "dev", "/workspace/model.bin", finalPath, CopyProgress{})
	if !errors.Is(err, errChecksumMismatch) {
		t.Fatalf("expected checksum mismatch, got %v", err)
	}
}

func TestBuildRemoteSingleFileSHA256CommandPrefersSha256sum(t *testing.T) {
	got := buildRemoteSingleFileSHA256Command("/workspace/model.bin")
	// sha256sum is the first probed primitive — it's in coreutils and
	// covers ~every Linux container including alpine. Python is only the
	// fallback.
	if !strings.Contains(got, "command -v sha256sum") {
		t.Fatalf("expected sha256sum probe in remote command, got: %s", got)
	}
	sumIdx := strings.Index(got, "command -v sha256sum")
	pyIdx := strings.Index(got, "command -v python3")
	if pyIdx == -1 || sumIdx == -1 || sumIdx >= pyIdx {
		t.Fatalf("expected sha256sum probe to precede python fallback, got: %s", got)
	}
	if !strings.Contains(got, "'/workspace/model.bin'") {
		t.Fatalf("expected shell-quoted remote path in command, got: %s", got)
	}
}

func runUploadScript(t *testing.T, script string, stdin io.Reader) (exitCode int, stderr string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	cmd.Stdin = stdin
	var errBuf bytes.Buffer
	cmd.Stderr = &errBuf
	err := cmd.Run()
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			t.Fatalf("run upload script: %v (stderr: %s)", err, errBuf.String())
		}
		return exitErr.ExitCode(), errBuf.String()
	}
	return 0, errBuf.String()
}

func TestBuildSingleFileUploadCommandWritesAtomicallyAndAcks(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "payload.bin")
	content := []byte("verified upload content")

	code, stderr := runUploadScript(t, buildSingleFileUploadCommand(dst, int64(len(content))), bytes.NewReader(content))
	if code != 0 {
		t.Fatalf("script exit = %d, stderr: %s", code, stderr)
	}
	if n, ok := parseUploadSizeMarker(stderr, "okdev-cp-ok:"); !ok || n != int64(len(content)) {
		t.Fatalf("expected okdev-cp-ok:%d marker, stderr: %s", len(content), stderr)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("dst content = %q, want %q", got, content)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".okdev-cp.") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

func TestBuildSingleFileUploadCommandRejectsShortStream(t *testing.T) {
	dir := t.TempDir()
	dst := filepath.Join(dir, "payload.bin")
	if err := os.WriteFile(dst, []byte("previous"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Feed 4 bytes while claiming 10 — the truncated-stream case.
	code, stderr := runUploadScript(t, buildSingleFileUploadCommand(dst, 10), strings.NewReader("abcd"))
	if code != 21 {
		t.Fatalf("script exit = %d, want 21, stderr: %s", code, stderr)
	}
	if n, ok := parseUploadSizeMarker(stderr, "okdev-cp-size:"); !ok || n != 4 {
		t.Fatalf("expected okdev-cp-size:4 marker, stderr: %s", stderr)
	}
	// The destination must keep its previous content on a failed upload.
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "previous" {
		t.Fatalf("dst content = %q, want untouched %q", got, "previous")
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".okdev-cp.") {
			t.Fatalf("temp file %s left behind", e.Name())
		}
	}
}

func TestBuildSingleFileUploadCommandRejectsDirectoryDestination(t *testing.T) {
	dir := t.TempDir()

	code, stderr := runUploadScript(t, buildSingleFileUploadCommand(dir, 4), strings.NewReader("abcd"))
	if code == 0 {
		t.Fatalf("expected non-zero exit for directory destination, stderr: %s", stderr)
	}
	if !strings.Contains(stderr, "is a directory") {
		t.Fatalf("expected 'is a directory' in stderr for non-retryable classification, got: %s", stderr)
	}
}

func TestBuildSingleFileUploadCommandPreservesExistingMode(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("mode preservation relies on GNU/busybox stat -c, exercised on linux")
	}
	dir := t.TempDir()
	dst := filepath.Join(dir, "script.sh")
	if err := os.WriteFile(dst, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	content := []byte("#!/bin/sh\necho updated\n")
	code, stderr := runUploadScript(t, buildSingleFileUploadCommand(dst, int64(len(content))), bytes.NewReader(content))
	if code != 0 {
		t.Fatalf("script exit = %d, stderr: %s", code, stderr)
	}
	info, err := os.Stat(dst)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o755 {
		t.Fatalf("dst mode = %o, want 755 preserved", info.Mode().Perm())
	}
}

func TestVerifySingleFileUploadResult(t *testing.T) {
	execFailure := errors.New("command terminated with exit code 21")
	cases := []struct {
		name      string
		execErr   error
		stderr    string
		size      int64
		wantNil   bool
		retryable bool
	}{
		{name: "acknowledged", stderr: "okdev-cp-ok:7\n", size: 7, wantNil: true},
		{name: "size mismatch is retryable", execErr: execFailure, stderr: "okdev-cp-size:3\n", size: 7, retryable: true},
		{name: "fake success without ack is retryable", stderr: "", size: 7, retryable: true},
		{name: "ack with wrong size is retryable", stderr: "okdev-cp-ok:3\n", size: 7, retryable: true},
		{name: "directory destination is not retryable", execErr: execFailure, stderr: "okdev-cp-err: /x is a directory\n", size: 7, retryable: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := verifySingleFileUploadResult(tc.execErr, tc.stderr, tc.size, "/x")
			if tc.wantNil {
				if err != nil {
					t.Fatalf("expected nil, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if got := isRetryableCopyStreamError(err); got != tc.retryable {
				t.Fatalf("retryable = %v, want %v (err: %v)", got, tc.retryable, err)
			}
		})
	}
}

func TestCopyToPodInContainerWithProgressRetriesDroppedStream(t *testing.T) {
	dir := t.TempDir()
	localPath := filepath.Join(dir, "payload.bin")
	content := "retried upload"
	if err := os.WriteFile(localPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := runPodUploadExecForCopy
	t.Cleanup(func() { runPodUploadExecForCopy = prev })

	attempts := 0
	var received []string
	runPodUploadExecForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, script string, stdin io.Reader, stderr io.Writer) error {
		attempts++
		data, err := io.ReadAll(stdin)
		if err != nil {
			return err
		}
		received = append(received, string(data))
		if attempts == 1 {
			// Dropped stream: exec exits cleanly, nothing ran remotely.
			return nil
		}
		fmt.Fprintf(stderr, "okdev-cp-ok:%d\n", len(data))
		return nil
	}

	client := &Client{}
	if err := client.CopyToPodInContainerWithProgress(context.Background(), "default", localPath, "pod-0", "dev", "/workspace/payload.bin", CopyProgress{}); err != nil {
		t.Fatalf("CopyToPodInContainerWithProgress: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if !reflect.DeepEqual(received, []string{content, content}) {
		t.Fatalf("received = %q, want full payload on both attempts", received)
	}
}

func TestCopyDirToPodWithProgressRetriesMissingAck(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	prev := runPodUploadExecForCopy
	t.Cleanup(func() { runPodUploadExecForCopy = prev })

	attempts := 0
	runPodUploadExecForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, script string, stdin io.Reader, stderr io.Writer) error {
		attempts++
		if _, err := io.Copy(io.Discard, stdin); err != nil {
			return err
		}
		if attempts == 1 {
			return nil
		}
		fmt.Fprintln(stderr, "okdev-cp-ok")
		return nil
	}

	client := &Client{}
	if err := client.CopyDirToPodWithProgress(context.Background(), "default", "pod-0", "dev", dir, "/workspace/dir", CopyProgress{}); err != nil {
		t.Fatalf("CopyDirToPodWithProgress: %v", err)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}

func TestCopyDirToPodOnceReportsLocalArchiveErrorAsRootCause(t *testing.T) {
	prev := runPodUploadExecForCopy
	t.Cleanup(func() { runPodUploadExecForCopy = prev })

	runPodUploadExecForCopy = func(_ context.Context, _ *Client, _ string, _ string, _ string, script string, stdin io.Reader, stderr io.Writer) error {
		if _, err := io.Copy(io.Discard, stdin); err != nil {
			// Mirror a remote tar dying on the truncated archive.
			return errors.New("command terminated with exit code 2")
		}
		fmt.Fprintln(stderr, "okdev-cp-ok")
		return nil
	}

	client := &Client{}
	err := client.CopyDirToPodWithProgress(context.Background(), "default", "pod-0", "dev", filepath.Join(t.TempDir(), "missing"), "/workspace/dir", CopyProgress{})
	if err == nil {
		t.Fatal("expected error for missing local directory")
	}
	if !strings.Contains(err.Error(), "no such file") {
		t.Fatalf("expected local archive error as root cause, got: %v", err)
	}
}
