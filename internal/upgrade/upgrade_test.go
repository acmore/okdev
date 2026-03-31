package upgrade

import (
	"net/http"
	"net/http/httptest"
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
