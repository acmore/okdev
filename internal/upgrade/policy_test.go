package upgrade

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestPolicyCheckerEnforcesVerifiedMinimumAndCachesIt(t *testing.T) {
	policy := `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	var server *httptest.Server
	fetches := 0
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fetches++
		switch r.URL.Path {
		case repoPath:
			fmt.Fprintf(w, `{"tag_name":"v1.3.0","assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, policy)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		now:        func() time.Time { return now },
		verify: func(_ context.Context, tag string, digest []byte) error {
			want := sha256.Sum256([]byte(policy))
			if tag != "v1.3.0" || string(digest) != string(want[:]) {
				return errors.New("wrong attestation subject")
			}
			return nil
		},
	}
	home := t.TempDir()
	checker.homeDir = func() (string, error) { return home, nil }

	if err := checker.Check(context.Background(), "v1.1.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version v1.2.0") {
		t.Fatalf("expected upgrade gate, got %v", err)
	}
	if err := checker.Check(context.Background(), "v1.2.0"); err != nil {
		t.Fatalf("minimum supported version rejected: %v", err)
	}
	if fetches != 2 {
		t.Fatalf("expected one release and one asset fetch, got %d", fetches)
	}

	now = now.Add(2 * time.Hour)
	server.Close()
	if err := checker.Check(context.Background(), "v1.3.0"); err == nil || !strings.Contains(err.Error(), "check minimum CLI version") {
		t.Fatalf("expired policy must fail closed, got %v", err)
	}
}

func TestPolicyCheckerRejectsMissingInvalidAndRolledBackPolicies(t *testing.T) {
	policy := `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	tag := "v1.3.0"
	includeAsset := true
	badSignature := false
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			if !includeAsset {
				fmt.Fprintf(w, `{"tag_name":%q,"assets":[]}`, tag)
				return
			}
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, tag, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, policy)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		now:        func() time.Time { return now },
		verify: func(context.Context, string, []byte) error {
			if badSignature {
				return errors.New("bad signature")
			}
			return nil
		},
	}
	home := t.TempDir()
	checker.homeDir = func() (string, error) { return home, nil }

	includeAsset = false
	if err := checker.Check(context.Background(), "v1.3.0"); err == nil || !strings.Contains(err.Error(), "has no release-policy.json") {
		t.Fatalf("missing policy accepted: %v", err)
	}
	includeAsset = true
	badSignature = true
	if err := checker.Check(context.Background(), "v1.3.0"); err == nil || !strings.Contains(err.Error(), "bad signature") {
		t.Fatalf("unverified policy accepted: %v", err)
	}
	badSignature = false
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.5.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.3.0"); err == nil || !strings.Contains(err.Error(), "invalid release policy") {
		t.Fatalf("floor above release accepted: %v", err)
	}
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.3.0"); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}
	now = now.Add(2 * time.Hour)
	tag = "v1.4.0"
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.1.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.3.0"); err == nil || !strings.Contains(err.Error(), "rollback") {
		t.Fatalf("lower minimum accepted: %v", err)
	}
}

func TestPrereleaseUsesOwnPolicyUntilStableReleaseHasOne(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			fmt.Fprint(w, `{"tag_name":"v0.14.1","assets":[]}`)
		case "/repos/acmore/okdev/releases/tags/v0.14.2-rc1":
			fmt.Fprintf(w, `{"tag_name":"v0.14.2-rc1","assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, `{"schemaVersion":1,"minimumCliVersion":"v0.14.1","enforceAfter":"2026-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return home, nil },
		now:        func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) },
		verify:     func(context.Context, string, []byte) error { return nil },
	}
	if err := checker.Check(context.Background(), "v0.14.2-rc1"); err != nil {
		t.Fatalf("prerelease rejected: %v", err)
	}
}

func TestPolicyCheckerEnforcesAfterGracePeriod(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			fmt.Fprintf(w, `{"tag_name":"v1.3.0","assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-11-13T00:30:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	home := t.TempDir()
	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return home, nil },
		now:        func() time.Time { return now },
		verify:     func(context.Context, string, []byte) error { return nil },
	}
	if err := checker.Check(context.Background(), "v1.1.0"); err != nil {
		t.Fatalf("floor enforced before grace period: %v", err)
	}
	now = now.Add(31 * time.Minute)
	if err := checker.Check(context.Background(), "v1.1.0"); err == nil || !strings.Contains(err.Error(), "minimum supported") {
		t.Fatalf("floor not enforced after grace period: %v", err)
	}
}
