package syncthing

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"
)

func TestChecksumForArchive(t *testing.T) {
	checksums := "abc123  syncthing-v1.2.3-linux-amd64.tar.gz\ndef456  other.tar.gz\n"
	got, err := checksumForArchive(checksums, "syncthing-v1.2.3-linux-amd64.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "abc123" {
		t.Fatalf("unexpected checksum %q", got)
	}
}

func TestChecksumForArchiveNotFound(t *testing.T) {
	if _, err := checksumForArchive("abc foo", "missing"); err == nil {
		t.Fatal("expected error")
	}
}

func TestSelectArchiveNameChoosesFirstMatch(t *testing.T) {
	checksums := "aaa  syncthing-linux-amd64-v1.2.3.tar.gz\nbbb  syncthing-macos-arm64-v1.2.3.zip\n"
	got, err := selectArchiveName(checksums, []string{"syncthing-macos-arm64-v1.2.3.zip", "syncthing-darwin-arm64-v1.2.3.tar.gz"})
	if err != nil {
		t.Fatal(err)
	}
	if got != "syncthing-macos-arm64-v1.2.3.zip" {
		t.Fatalf("unexpected archive %q", got)
	}
}

func TestSelectArchiveNameNotFound(t *testing.T) {
	if _, err := selectArchiveName("aaa syncthing-linux-amd64-v1.2.3.tar.gz", []string{"syncthing-macos-arm64-v1.2.3.zip"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestLooksLikeSyncthingBinaryPath(t *testing.T) {
	if !looksLikeSyncthingBinaryPath("syncthing-macos-arm64-v1.2.3/syncthing", 20<<20) {
		t.Fatal("expected binary path match")
	}
	if looksLikeSyncthingBinaryPath("syncthing-macos-arm64-v1.2.3/etc/firewall-ufw/syncthing", 175) {
		t.Fatal("did not expect helper file to match")
	}
	if looksLikeSyncthingBinaryPath("syncthing", 120) {
		t.Fatal("did not expect tiny file to match")
	}
}

type fakeHTTPDoer struct {
	resp *http.Response
	err  error
}

func (f fakeHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return f.resp, f.err
}

func TestHTTPGetUsesInjectableClient(t *testing.T) {
	old := installerHTTPClient
	t.Cleanup(func() { installerHTTPClient = old })
	installerHTTPClient = fakeHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewBufferString("ok")),
		},
	}
	got, err := httpGet(context.Background(), "https://example.com/file")
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "ok" {
		t.Fatalf("unexpected body %q", string(got))
	}
}

func TestDownloadArchiveToTemp(t *testing.T) {
	body := []byte("archive-bytes")
	old := installerHTTPClient
	t.Cleanup(func() { installerHTTPClient = old })
	installerHTTPClient = fakeHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(bytes.NewReader(body)),
		},
	}

	path, gotChecksum, err := downloadArchiveToTemp(context.Background(), "https://example.com/archive", "syncthing.tar.gz")
	if err != nil {
		t.Fatal(err)
	}
	defer os.Remove(path)

	gotBody, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(gotBody, body) {
		t.Fatalf("unexpected archive body %q", gotBody)
	}
	wantChecksum := fmt.Sprintf("%x", sha256.Sum256(body))
	if gotChecksum != wantChecksum {
		t.Fatalf("unexpected checksum got=%q want=%q", gotChecksum, wantChecksum)
	}
}

func TestExtractSyncthingBinaryFromTarGzToPath(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "syncthing.tar.gz")
	if err := writeTarGzArchive(archivePath, "syncthing-linux-amd64-v1.2.3/syncthing", bytes.Repeat([]byte("x"), 2<<20)); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "syncthing")
	if err := extractSyncthingBinaryFromTarGzToPath(archivePath, outPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2<<20 {
		t.Fatalf("unexpected extracted size %d", info.Size())
	}
}

func TestExtractSyncthingBinaryFromZipToPath(t *testing.T) {
	dir := t.TempDir()
	archivePath := filepath.Join(dir, "syncthing.zip")
	if err := writeZipArchive(archivePath, "syncthing-macos-arm64-v1.2.3/syncthing", bytes.Repeat([]byte("z"), 2<<20)); err != nil {
		t.Fatal(err)
	}

	outPath := filepath.Join(dir, "syncthing")
	if err := extractSyncthingBinaryFromZipToPath(archivePath, outPath); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(outPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 2<<20 {
		t.Fatalf("unexpected extracted size %d", info.Size())
	}
}

func TestLookupInPath(t *testing.T) {
	dir := t.TempDir()
	binPath := filepath.Join(dir, "syncthing")
	if err := os.WriteFile(binPath, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	origPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", origPath) })

	got, err := lookupInPath("syncthing")
	if err != nil {
		t.Fatal(err)
	}
	if got != binPath {
		t.Fatalf("unexpected path %q", got)
	}
}

func writeTarGzArchive(path, name string, content []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	gz := gzip.NewWriter(f)
	defer gz.Close()
	tw := tar.NewWriter(gz)
	defer tw.Close()
	hdr := &tar.Header{
		Name: name,
		Mode: 0o755,
		Size: int64(len(content)),
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	_, err = tw.Write(content)
	return err
}

func writeZipArchive(path, name string, content []byte) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	zw := zip.NewWriter(f)
	defer zw.Close()
	w, err := zw.Create(name)
	if err != nil {
		return err
	}
	_, err = w.Write(content)
	return err
}
