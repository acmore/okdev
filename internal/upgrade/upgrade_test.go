package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLatestVersionFetchesFromGitHub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acmore/okdev/releases/latest" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v1.2.3"}`))
	}))
	defer server.Close()

	c := &Checker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return t.TempDir(), nil },
		now:        time.Now,
	}
	v, err := c.LatestVersion()
	if err != nil {
		t.Fatalf("LatestVersion: %v", err)
	}
	if v != "v1.2.3" {
		t.Fatalf("expected v1.2.3, got %s", v)
	}
}

func TestLatestVersionUsesCache(t *testing.T) {
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	defer server.Close()

	home := t.TempDir()
	now := time.Date(2026, 3, 31, 12, 0, 0, 0, time.UTC)
	c := &Checker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return home, nil },
		now:        func() time.Time { return now },
	}

	// First call fetches from server.
	v1, err := c.LatestVersionCached()
	if err != nil {
		t.Fatalf("first call: %v", err)
	}
	if v1 != "v2.0.0" {
		t.Fatalf("expected v2.0.0, got %s", v1)
	}
	if calls != 1 {
		t.Fatalf("expected 1 API call, got %d", calls)
	}

	// Second call within TTL uses cache.
	v2, err := c.LatestVersionCached()
	if err != nil {
		t.Fatalf("second call: %v", err)
	}
	if v2 != "v2.0.0" {
		t.Fatalf("expected v2.0.0, got %s", v2)
	}
	if calls != 1 {
		t.Fatalf("expected still 1 API call, got %d", calls)
	}

	// After TTL expires, fetches again.
	c.now = func() time.Time { return now.Add(25 * time.Hour) }
	v3, err := c.LatestVersionCached()
	if err != nil {
		t.Fatalf("third call: %v", err)
	}
	if v3 != "v2.0.0" {
		t.Fatalf("expected v2.0.0, got %s", v3)
	}
	if calls != 2 {
		t.Fatalf("expected 2 API calls, got %d", calls)
	}
}

func TestDownloadAndReplace(t *testing.T) {
	// Create a fake binary tarball.
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("#!/bin/sh\necho okdev-new")
	hdr := &tar.Header{Name: "okdev", Size: int64(len(content)), Mode: 0o755}
	tw.WriteHeader(hdr)
	tw.Write(content)
	tw.Close()

	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(tarBuf.Bytes())
	gw.Close()

	tarGzBytes := gzBuf.Bytes()
	checksum := sha256.Sum256(tarGzBytes)
	checksumHex := hex.EncodeToString(checksum[:])
	checksumBody := checksumHex + "  okdev_linux_amd64.tar.gz\n"

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(tarGzBytes)
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			w.Write([]byte(checksumBody))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	binDir := t.TempDir()
	binPath := filepath.Join(binDir, "okdev")
	os.WriteFile(binPath, []byte("old"), 0o755)

	u := &Upgrader{
		assetBaseURL: server.URL + "/",
		httpClient:   server.Client(),
		goos:         "linux",
		goarch:       "amd64",
		binPath:      binPath,
	}
	err := u.Upgrade()
	if err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(content) {
		t.Fatalf("expected new binary content, got %q", string(got))
	}
}

func TestCheckAndRemindPrintsWhenNewer(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v2.0.0"}`))
	}))
	defer server.Close()

	var buf bytes.Buffer
	c := &Checker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return t.TempDir(), nil },
		now:        time.Now,
		launch:     func(fn func()) { fn() },
	}
	c.CheckAndRemind("v1.0.0", &buf)
	if !strings.Contains(buf.String(), "okdev upgrade") {
		t.Fatalf("expected upgrade reminder, got %q", buf.String())
	}
}

func TestCheckAndRemindSilentWhenCurrent(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"tag_name":"v1.0.0"}`))
	}))
	defer server.Close()

	var buf bytes.Buffer
	c := &Checker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return t.TempDir(), nil },
		now:        time.Now,
		launch:     func(fn func()) { fn() },
	}
	c.CheckAndRemind("v1.0.0", &buf)
	if buf.String() != "" {
		t.Fatalf("expected no output, got %q", buf.String())
	}
}
