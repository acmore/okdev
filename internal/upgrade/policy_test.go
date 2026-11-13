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

	// Once the policy expires and GitHub is unreachable, the last verified
	// floor keeps applying and the failed refresh is not retried per command.
	now = now.Add(policyCheckInterval + time.Minute)
	server.Close()
	if err := checker.Check(context.Background(), "v1.3.0"); err != nil {
		t.Fatalf("unreachable policy must not block a supported version: %v", err)
	}
	if err := checker.Check(context.Background(), "v1.1.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version v1.2.0") {
		t.Fatalf("stale policy must still enforce the known floor, got %v", err)
	}
	entry, err := checker.readCache()
	if err != nil || !entry.RetryAt.Equal(now.Add(policyCheckInterval)) {
		t.Fatalf("failed refresh must back off: entry=%+v err=%v", entry, err)
	}
}

func TestPolicyCheckerFailsOpenWithoutVerifiedPolicy(t *testing.T) {
	policy := `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	tag := "v1.3.0"
	includeAsset := true
	badSignature := false
	releaseFetches := 0
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			releaseFetches++
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
	var logs []string
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
		logf: func(format string, args ...any) { logs = append(logs, fmt.Sprintf(format, args...)) },
	}
	home := t.TempDir()
	checker.homeDir = func() (string, error) { return home, nil }
	expectOpen := func(reason string) {
		t.Helper()
		logs = nil
		if err := checker.Check(context.Background(), "v1.0.0"); err != nil {
			t.Fatalf("%s must not block: %v", reason, err)
		}
		if len(logs) != 1 || !strings.Contains(logs[0], reason) {
			t.Fatalf("expected %q in log, got %q", reason, logs)
		}
		now = now.Add(policyCheckInterval)
	}

	includeAsset = false
	expectOpen("has no release-policy.json")
	// The failure is cached: a second command inside the check interval does
	// not hit the API again.
	now = now.Add(-policyCheckInterval / 2)
	before := releaseFetches
	if err := checker.Check(context.Background(), "v1.0.0"); err != nil || releaseFetches != before {
		t.Fatalf("failed refresh retried too early: err=%v fetches=%d", err, releaseFetches-before)
	}
	now = now.Add(policyCheckInterval)

	includeAsset = true
	badSignature = true
	expectOpen("bad signature")
	badSignature = false
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.5.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	expectOpen("invalid release policy")

	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.0.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version v1.2.0") {
		t.Fatalf("verified policy must be enforced, got %v", err)
	}
}

func TestPolicyCheckerKeepsFloorButAcceptsWithdrawnRelease(t *testing.T) {
	policy := `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	tag := "v1.4.0"
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			fmt.Fprintf(w, `{"tag_name":%q,"assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, tag, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, policy)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	now := time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC)
	home := t.TempDir()
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return home, nil },
		now:        func() time.Time { return now },
		verify:     func(context.Context, string, []byte) error { return nil },
	}
	if err := checker.Check(context.Background(), "v1.2.0"); err != nil {
		t.Fatalf("valid policy rejected: %v", err)
	}

	// v1.4.0 is withdrawn, so latest falls back to v1.3.0 and replays a lower
	// floor. The older tag is accepted; the lower floor is not.
	now = now.Add(policyCheckInterval + time.Minute)
	tag = "v1.3.0"
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.1.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.2.0"); err != nil {
		t.Fatalf("withdrawn release must not block a supported version: %v", err)
	}
	err := checker.Check(context.Background(), "v1.1.0")
	if err == nil || !strings.Contains(err.Error(), "minimum supported version v1.2.0 (latest: v1.3.0)") {
		t.Fatalf("lower floor must not relax the cached one, got %v", err)
	}

	now = now.Add(policyCheckInterval + time.Minute)
	policy = `{"schemaVersion":1,"minimumCliVersion":"v1.3.0","enforceAfter":"2026-01-01T00:00:00Z"}`
	if err := checker.Check(context.Background(), "v1.2.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version v1.3.0") {
		t.Fatalf("raised floor not adopted, got %v", err)
	}
}

func TestPolicyCheckerWorksWithoutWritableCache(t *testing.T) {
	var server *httptest.Server
	server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case repoPath:
			fmt.Fprintf(w, `{"tag_name":"v1.3.0","assets":[{"name":"release-policy.json","browser_download_url":%q}]}`, server.URL+"/policy")
		case "/policy":
			fmt.Fprint(w, `{"schemaVersion":1,"minimumCliVersion":"v1.2.0","enforceAfter":"2026-01-01T00:00:00Z"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	checker := &PolicyChecker{
		apiBase:    server.URL,
		httpClient: server.Client(),
		homeDir:    func() (string, error) { return "", errors.New("no home directory") },
		now:        func() time.Time { return time.Date(2026, 11, 13, 0, 0, 0, 0, time.UTC) },
		verify:     func(context.Context, string, []byte) error { return nil },
	}
	if err := checker.Check(context.Background(), "v1.2.0"); err != nil {
		t.Fatalf("unwritable cache must not block: %v", err)
	}
	if err := checker.Check(context.Background(), "v1.1.0"); err == nil || !strings.Contains(err.Error(), "minimum supported version v1.2.0") {
		t.Fatalf("policy must still be enforced without a cache, got %v", err)
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
