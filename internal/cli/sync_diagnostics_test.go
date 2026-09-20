package cli

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestSyncHealthRequiresLiveAPIAndPeer(t *testing.T) {
	for _, tc := range []struct {
		name, folders, connections string
		code                       int
		want                       syncHealthStatus
	}{
		{"healthy", `[{"id":"okdev-test"}]`, `{"connections":{"peer":{"connected":true}}}`, 200, syncHealthActive},
		{"disconnected", `[{"id":"okdev-test"}]`, `{"connections":{}}`, 200, syncHealthStale},
		{"paused", `[{"id":"okdev-test","paused":true}]`, `{"connections":{}}`, 200, syncHealthPaused},
		{"wrong session", `[{"id":"okdev-other"}]`, `{"connections":{"peer":{"connected":true}}}`, 200, syncHealthStale},
		{"API rejected", `[]`, `{}`, 403, syncHealthStale},
		{"malformed connections", `[{"id":"okdev-test"}]`, `invalid`, 200, syncHealthStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(tc.code)
				if r.URL.Path == "/rest/config" {
					fmt.Fprintf(w, `{"folders":%s}`, tc.folders)
				} else {
					fmt.Fprint(w, tc.connections)
				}
			}))
			defer server.Close()
			got, reason := checkSyncthingAPIHealth(context.Background(), "test", server.URL, "key")
			if got != tc.want {
				t.Fatalf("got %s (%s), want %s", got, reason, tc.want)
			}
			server.Close()
			got, _ = checkSyncthingAPIHealth(context.Background(), "test", server.URL, "key")
			if got != syncHealthStale {
				t.Fatalf("closed API reported %s", got)
			}
		})
	}
}

func TestSyncExitEvidenceMatchesProcessAndOmitsRawError(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	recordSyncExit("test", "", errors.New("secret-token"))
	record := readSyncExit("test")
	if record == nil || record.PID != os.Getpid() || record.LogPath == "" {
		t.Fatalf("bad record: %+v", record)
	}
	if strings.Contains(record.Reason, "secret-token") {
		t.Fatal("raw error leaked")
	}
	if got := stoppedSyncReason("test", record.PID); !strings.Contains(got, record.Reason) {
		t.Fatal(got)
	}
	if got := stoppedSyncReason("test", record.PID+1); !strings.Contains(got, "exit cause unavailable") {
		t.Fatal(got)
	}
	err := waitForReadySync(context.Background(), "missing", time.Second)
	if err == nil || !strings.Contains(err.Error(), "workload retained") {
		t.Fatalf("missing sync accepted: %v", err)
	}
}

func TestSyncRepeatedWarningsRemainVisibleAndReset(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	full := syncPreflightWarning("test", syncHealthStopped, "dead", "exec")
	compact := syncPreflightWarning("test", syncHealthStopped, "dead", "exec")
	if !strings.Contains(full, "warning:") || !strings.Contains(compact, "sync still unhealthy") || !strings.Contains(compact, "may run stale code") || len(compact) >= len(full) {
		t.Fatalf("full=%q compact=%q", full, compact)
	}
	changed := syncPreflightWarning("test", syncHealthStale, "API lost", "exec")
	if strings.Contains(changed, "still unhealthy") {
		t.Fatal("new fault compacted")
	}
	syncPreflightWarning("test", syncHealthActive, "", "exec")
	if got := syncPreflightWarning("test", syncHealthStopped, "dead", "exec"); got != full {
		t.Fatal(got)
	}
	path := syncDiagnosticPath("test", "sync.warning")
	if err := writeSyncDiagnostic(path, []byte(fmt.Sprintf("%d\nstopped\ndead", time.Now().Add(-2*time.Minute).Unix()))); err != nil {
		t.Fatal(err)
	}
	if got := syncPreflightWarning("test", syncHealthStopped, "dead", "exec"); got != full {
		t.Fatal(got)
	}
}
