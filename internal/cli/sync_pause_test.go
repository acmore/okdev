package cli

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

func TestCountPausedSessionFolders(t *testing.T) {
	cfg := map[string]any{"folders": []any{
		map[string]any{"id": "okdev-sess", "paused": true},
		map[string]any{"id": "okdev-sess-ab12cd34", "paused": false},
		map[string]any{"id": "okdev-other", "paused": true},    // other session
		map[string]any{"id": "okdev-sessions", "paused": true}, // prefix trap: not ours
		map[string]any{"id": "default"},
	}}
	paused, total := countPausedSessionFolders(cfg, "sess")
	if paused != 1 || total != 2 {
		t.Fatalf("paused=%d total=%d, want 1/2", paused, total)
	}
	if sessionFolderIDMatches("okdev-sessions", "sess") {
		t.Fatal("okdev-sessions must not match session sess")
	}
}

func TestSetSessionSyncPausedViaPatchesOnlyManagedFolders(t *testing.T) {
	var mu sync.Mutex
	patched := map[string]bool{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
			_ = json.NewEncoder(w).Encode(map[string]any{"folders": []any{
				map[string]any{"id": "okdev-sess"},
				map[string]any{"id": "okdev-sess-ab12cd34"},
				map[string]any{"id": "okdev-other"},
			}})
		case r.Method == http.MethodPatch && strings.HasPrefix(r.URL.Path, "/rest/config/folders/"):
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			mu.Lock()
			patched[strings.TrimPrefix(r.URL.Path, "/rest/config/folders/")] = body["paused"] == true
			mu.Unlock()
			w.WriteHeader(http.StatusOK)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	touched, err := setSessionSyncPausedVia(context.Background(), server.URL, "test-key", "sess", true)
	if err != nil {
		t.Fatalf("setSessionSyncPausedVia: %v", err)
	}
	if touched != 2 {
		t.Fatalf("touched=%d, want 2", touched)
	}
	mu.Lock()
	defer mu.Unlock()
	if !patched["okdev-sess"] || !patched["okdev-sess-ab12cd34"] {
		t.Fatalf("managed folders must be paused, got %v", patched)
	}
	if _, ok := patched["okdev-other"]; ok {
		t.Fatalf("foreign session folder must not be touched, got %v", patched)
	}
}

func TestSyncWaitGateAndDetachWarningForPaused(t *testing.T) {
	err := syncWaitGateError("sess", syncHealthPaused, "")
	if err == nil || !strings.Contains(err.Error(), "okdev sync resume") {
		t.Fatalf("paused gate must point at resume, got %v", err)
	}
	if strings.Contains(err.Error(), "repair") {
		t.Fatalf("paused gate must not suggest repair, got %v", err)
	}
	warn := detachSyncWarning(syncHealthPaused, "")
	if !strings.Contains(warn, "paused") || !strings.Contains(warn, "resume") {
		t.Fatalf("detach warning must explain paused state, got %q", warn)
	}
}
