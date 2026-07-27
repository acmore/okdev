package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

// /rest/db/scan is synchronous, so a big folder outlives the HTTP client
// timeout. Treating that as a failure broke `okdev sync wait` on exactly the
// large transfers it exists for, while the scan was in fact still running.
func TestIsSyncthingScanStillRunning(t *testing.T) {
	timeoutish := []error{
		context.DeadlineExceeded,
		fmt.Errorf("wrapped: %w", context.DeadlineExceeded),
		&url.Error{Op: "Post", URL: "http://127.0.0.1/rest/db/scan", Err: context.DeadlineExceeded},
		errors.New(`Post "http://127.0.0.1:1/rest/db/scan": context deadline exceeded (Client.Timeout exceeded while awaiting headers)`),
	}
	for _, err := range timeoutish {
		if !isSyncthingScanStillRunning(err) {
			t.Fatalf("expected %v to read as a scan still running", err)
		}
	}

	// Anything else is a real failure and must still abort the wait: a wrong
	// API key or a dead endpoint means the rescan never started.
	for _, err := range []error{
		nil,
		errors.New("connection refused"),
		errors.New("syncthing api 403: forbidden"),
	} {
		if isSyncthingScanStillRunning(err) {
			t.Fatalf("expected %v to stay fatal", err)
		}
	}
}

// The classifier has to hold against the error the real HTTP client actually
// produces, not just against hand-written ones: /rest/db/scan is synchronous,
// so a folder that takes longer to hash than the client timeout returns a
// *url.Error from net/http, and misreading that as fatal is what broke
// `okdev sync wait` on large transfers.
func TestScanFolderTimeoutIsClassifiedAsStillRunning(t *testing.T) {
	// Bounded, not indefinite: httptest's Close waits for in-flight handlers,
	// so a handler that blocks forever deadlocks the test rather than the
	// server under test.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(2 * time.Second) // outlives the client timeout below
	}))
	defer srv.Close()

	previous := syncthingHTTPClient
	syncthingHTTPClient = &http.Client{Timeout: 150 * time.Millisecond}
	defer func() { syncthingHTTPClient = previous }()

	err := syncthingScanFolder(context.Background(), srv.URL, "key", "okdev-sess")
	if err == nil {
		t.Fatal("expected the scan call to time out")
	}
	if !isSyncthingScanStillRunning(err) {
		t.Fatalf("real client timeout must read as still-running, got %#v", err)
	}
}

// A server that refuses outright never started a scan, so the wait must abort
// rather than pretend indexing is under way.
func TestScanFolderHardFailureStaysFatal(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "forbidden", http.StatusForbidden)
	}))
	defer srv.Close()

	err := syncthingScanFolder(context.Background(), srv.URL, "key", "okdev-sess")
	if err == nil {
		t.Fatal("expected an error for a 403 response")
	}
	if isSyncthingScanStillRunning(err) {
		t.Fatalf("a refused scan must stay fatal, got %v", err)
	}
}
