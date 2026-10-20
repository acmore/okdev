package upgrade

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sigstore/sigstore-go/pkg/root"
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

// fakeReleaseServer serves a release archive, its checksums, and the
// attestation API (attestations is the raw API body; "" means 404).
func fakeReleaseServer(t *testing.T, attestations string) (*httptest.Server, []byte) {
	t.Helper()
	var tarBuf bytes.Buffer
	tw := tar.NewWriter(&tarBuf)
	content := []byte("#!/bin/sh\necho okdev-new")
	tw.WriteHeader(&tar.Header{Name: "okdev", Size: int64(len(content)), Mode: 0o755})
	tw.Write(content)
	tw.Close()
	var gzBuf bytes.Buffer
	gw := gzip.NewWriter(&gzBuf)
	gw.Write(tarBuf.Bytes())
	gw.Close()
	tarGzBytes := gzBuf.Bytes()
	checksum := sha256.Sum256(tarGzBytes)
	checksumBody := hex.EncodeToString(checksum[:]) + "  okdev_linux_amd64.tar.gz\n"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/attestations/sha256:"+hex.EncodeToString(checksum[:])):
			if attestations == "" {
				http.NotFound(w, r)
				return
			}
			w.Write([]byte(attestations))
		case strings.HasSuffix(r.URL.Path, ".tar.gz"):
			w.Write(tarGzBytes)
		case strings.HasSuffix(r.URL.Path, "checksums.txt"):
			w.Write([]byte(checksumBody))
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(server.Close)
	return server, content
}

func newTestUpgrader(t *testing.T, server *httptest.Server) (*Upgrader, string) {
	t.Helper()
	binPath := filepath.Join(t.TempDir(), "okdev")
	os.WriteFile(binPath, []byte("old"), 0o755)
	trusted := func() (root.TrustedMaterial, error) {
		data, err := os.ReadFile("testdata/sigstore-public-good-trusted-root.json")
		if err != nil {
			return nil, err
		}
		return root.NewTrustedRootFromJSON(data)
	}
	return &Upgrader{
		assetBaseURL: server.URL + "/",
		httpClient:   server.Client(),
		goos:         "linux",
		goarch:       "amd64",
		binPath:      binPath,
		version:      "v9.9.9",
		apiBase:      server.URL,
		trustedRoot:  trusted,
		Warn:         io.Discard,
	}, binPath
}

func TestUpgradeRefusesReleaseWithoutAttestation(t *testing.T) {
	server, _ := fakeReleaseServer(t, "")
	u, binPath := newTestUpgrader(t, server)
	if err := u.Upgrade(); err == nil || !errors.Is(err, ErrNoAttestation) {
		t.Fatalf("expected ErrNoAttestation, got %v", err)
	}
	if got, _ := os.ReadFile(binPath); string(got) != "old" {
		t.Fatal("binary replaced without provenance")
	}
}

func TestUpgradeRefusesUnverifiableAttestationEvenWhenAllowed(t *testing.T) {
	// An attestation exists but is not okdev's (here: a genuine cli/cli
	// bundle for another digest). --allow-unattested must not bypass it.
	bundle, err := os.ReadFile("testdata/gh-cli-provenance-attestation.json")
	if err != nil {
		t.Fatal(err)
	}
	server, _ := fakeReleaseServer(t, `{"attestations":[{"bundle":`+string(bundle)+`}]}`)
	u, binPath := newTestUpgrader(t, server)
	u.AllowUnattested = true
	if err := u.Upgrade(); err == nil || errors.Is(err, ErrNoAttestation) {
		t.Fatalf("expected a verification failure, got %v", err)
	}
	if got, _ := os.ReadFile(binPath); string(got) != "old" {
		t.Fatal("binary replaced despite a failed verification")
	}
}

func TestDownloadAndReplace(t *testing.T) {
	// A release predating attestations installs only with the explicit
	// opt-out, and says so.
	server, content := fakeReleaseServer(t, "")
	u, binPath := newTestUpgrader(t, server)
	u.AllowUnattested = true
	var warn bytes.Buffer
	u.Warn = &warn
	if err := u.Upgrade(); err != nil {
		t.Fatalf("Upgrade: %v", err)
	}
	got, _ := os.ReadFile(binPath)
	if string(got) != string(content) {
		t.Fatalf("expected new binary content, got %q", string(got))
	}
	if !strings.Contains(warn.String(), "no build provenance attestation") {
		t.Fatalf("expected an unattested warning, got %q", warn.String())
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

func TestCheckAndRemindSilentWhenCurrentIsNewerPrerelease(t *testing.T) {
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
	c.CheckAndRemind("v1.1.0-rc1", &buf)
	if buf.String() != "" {
		t.Fatalf("expected no output for newer prerelease, got %q", buf.String())
	}
}

func TestShouldUpgrade(t *testing.T) {
	cases := []struct {
		current string
		latest  string
		want    bool
	}{
		{current: "v1.0.0", latest: "v1.0.0", want: false},
		{current: "1.0.0", latest: "v1.0.1", want: true},
		{current: "v1.1.0-rc1", latest: "v1.0.0", want: false},
		{current: "v1.0.0-rc1", latest: "v1.0.0", want: true},
		{current: "0.0.0-dev", latest: "v0.5.2", want: true},
	}
	for _, tc := range cases {
		if got := ShouldUpgrade(tc.current, tc.latest); got != tc.want {
			t.Fatalf("ShouldUpgrade(%q, %q)=%v want %v", tc.current, tc.latest, got, tc.want)
		}
	}
}

func TestResolveBinaryPathRejectsSymlinkedInvocation(t *testing.T) {
	tmp := t.TempDir()
	target := filepath.Join(tmp, "okdev-real")
	link := filepath.Join(tmp, "okdev")
	if err := os.WriteFile(target, []byte("real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}

	_, err := resolveBinaryPath(
		func() (string, error) { return target, nil },
		func(string) (string, error) { return link, nil },
		os.Lstat,
		filepath.EvalSymlinks,
		[]string{"okdev"},
	)
	if err == nil || !strings.Contains(err.Error(), "invoked through symlink") {
		t.Fatalf("expected symlink rejection, got %v", err)
	}
}

type fakeFileInfo struct{ mode fs.FileMode }

func (f fakeFileInfo) Name() string       { return "okdev" }
func (f fakeFileInfo) Size() int64        { return 0 }
func (f fakeFileInfo) Mode() fs.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return false }
func (f fakeFileInfo) Sys() any           { return nil }

func TestResolveBinaryPathPrefersInvokedNonSymlinkPath(t *testing.T) {
	got, err := resolveBinaryPath(
		func() (string, error) { return "/opt/okdev/bin/okdev", nil },
		func(string) (string, error) { return "/usr/local/bin/okdev", nil },
		func(string) (fs.FileInfo, error) { return fakeFileInfo{mode: 0o755}, nil },
		func(path string) (string, error) {
			if path == "/usr/local/bin/okdev" {
				return "/opt/okdev/bin/okdev", nil
			}
			return path, nil
		},
		[]string{"okdev"},
	)
	if err != nil {
		t.Fatalf("resolveBinaryPath: %v", err)
	}
	if got != "/usr/local/bin/okdev" {
		t.Fatalf("expected invoked path, got %q", got)
	}
}
