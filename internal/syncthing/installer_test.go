package syncthing

import (
	"bytes"
	"context"
	"io"
	"net/http"
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
