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
