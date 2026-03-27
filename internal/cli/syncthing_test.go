package cli

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
)

func TestSyncthingObjectArray(t *testing.T) {
	arr, err := syncthingObjectArray(map[string]any{"devices": []any{map[string]any{"deviceID": "abc"}}}, "devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(arr) != 1 {
		t.Fatalf("unexpected array length %d", len(arr))
	}
}

func TestSyncthingObjectArrayRejectsWrongType(t *testing.T) {
	_, err := syncthingObjectArray(map[string]any{"devices": map[string]any{}}, "devices")
	if err == nil {
		t.Fatal("expected type error")
	}
}

func TestSyncthingObjectMapRejectsWrongType(t *testing.T) {
	_, err := syncthingObjectMap("not-a-map", "devices")
	if err == nil {
		t.Fatal("expected type error")
	}
}

func TestSyncthingCompletion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/db/completion" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("folder"); got != "okdev-sess" {
			t.Fatalf("unexpected folder query %q", got)
		}
		if got := r.URL.Query().Get("device"); got != "REMOTEID" {
			t.Fatalf("unexpected device query %q", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "k" {
			t.Fatalf("unexpected API key %q", got)
		}
		_, _ = w.Write([]byte(`{"completion":99.5,"needBytes":1234}`))
	}))
	defer srv.Close()

	pct, need, err := syncthingCompletion(context.Background(), srv.URL, "k", "okdev-sess", "REMOTEID")
	if err != nil {
		t.Fatal(err)
	}
	if pct != 99.5 || need != 1234 {
		t.Fatalf("unexpected completion values pct=%v need=%d", pct, need)
	}
}

func TestBuildSTIgnoreContent(t *testing.T) {
	content, ok := buildSTIgnoreContent([]string{" .git/ ", "", "node_modules/"})
	if !ok {
		t.Fatal("expected content")
	}
	if content != ".git/\nnode_modules/\n" {
		t.Fatalf("unexpected content %q", content)
	}
}

func TestApplyManagedSyncthingFolderDefaults(t *testing.T) {
	folder := map[string]any{
		"id": "okdev-test",
	}

	applyManagedSyncthingFolderDefaults(folder, 300)

	if got := folder["fsWatcherEnabled"]; got != true {
		t.Fatalf("expected fsWatcherEnabled=true, got %#v", got)
	}
	if got := folder["fsWatcherDelayS"]; got != syncthingWatcherDelayS {
		t.Fatalf("expected fsWatcherDelayS=%d, got %#v", syncthingWatcherDelayS, got)
	}
	if got := folder["rescanIntervalS"]; got != 300 {
		t.Fatalf("expected rescanIntervalS=%d, got %#v", 300, got)
	}
}

func TestLocalSyncthingLogPath(t *testing.T) {
	got, err := localSyncthingLogPath("/tmp/okdev-session")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join("/tmp/okdev-session", "local.log")
	if got != want {
		t.Fatalf("unexpected log path: got %q want %q", got, want)
	}
}
