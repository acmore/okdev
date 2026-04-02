package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
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

func TestSyncthingFolderNeedBytes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/db/status" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("folder"); got != "okdev-sess" {
			t.Fatalf("unexpected folder query %q", got)
		}
		_, _ = w.Write([]byte(`{"needBytes":5678}`))
	}))
	defer srv.Close()

	need, err := syncthingFolderNeedBytes(context.Background(), srv.URL, "k", "okdev-sess")
	if err != nil {
		t.Fatal(err)
	}
	if need != 5678 {
		t.Fatalf("unexpected needBytes=%d", need)
	}
}

func TestSyncthingFolderNeedBytesZeroWhenSynced(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"needBytes":0}`))
	}))
	defer srv.Close()

	need, err := syncthingFolderNeedBytes(context.Background(), srv.URL, "k", "okdev-sess")
	if err != nil {
		t.Fatal(err)
	}
	if need != 0 {
		t.Fatalf("expected 0, got needBytes=%d", need)
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

func TestWriteSTIgnore(t *testing.T) {
	dir := t.TempDir()
	if err := writeSTIgnore(dir, []string{".git/", "node_modules/"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".stignore"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ".git/\nnode_modules/\n" {
		t.Fatalf("unexpected stignore content %q", got)
	}
}

func TestWriteLocalSTIgnoreWritesDefaultsWhenMissing(t *testing.T) {
	dir := t.TempDir()
	if err := writeLocalSTIgnore(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".stignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range defaultSyncExcludes {
		if !strings.Contains(string(got), pattern) {
			t.Fatalf("expected default exclude %q in stignore, got %q", pattern, got)
		}
	}
}

func TestWriteLocalSTIgnorePreservesExistingFileWhenDefaultsImplicit(t *testing.T) {
	dir := t.TempDir()
	stignorePath := filepath.Join(dir, ".stignore")
	if err := os.WriteFile(stignorePath, []byte("custom/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSTIgnore(dir, nil); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stignorePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "custom/\n" {
		t.Fatalf("expected existing stignore to be preserved, got %q", got)
	}
}

func TestWriteLocalSTIgnoreOverridesExistingFileWhenExcludesExplicit(t *testing.T) {
	dir := t.TempDir()
	stignorePath := filepath.Join(dir, ".stignore")
	if err := os.WriteFile(stignorePath, []byte("custom/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSTIgnore(dir, []string{".git/"}); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(stignorePath)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != ".git/\n" {
		t.Fatalf("expected explicit excludes to replace file, got %q", got)
	}
}

type syncthingExecRecorder struct {
	namespace string
	pod       string
	container string
	script    string
	out       []byte
	err       error
}

func (r *syncthingExecRecorder) ExecShInContainer(_ context.Context, namespace, pod, container, script string) ([]byte, error) {
	r.namespace = namespace
	r.pod = pod
	r.container = container
	r.script = script
	return r.out, r.err
}

func TestWriteRemoteSTIgnoreInPod(t *testing.T) {
	rec := &syncthingExecRecorder{}
	err := writeRemoteSTIgnoreInPod(context.Background(), rec, "ns", "pod", "/workspace", []string{".git/", "node_modules/"})
	if err != nil {
		t.Fatal(err)
	}
	if rec.namespace != "ns" || rec.pod != "pod" || rec.container != syncthingContainerName {
		t.Fatalf("unexpected exec target: %+v", rec)
	}
	if !strings.Contains(rec.script, "/workspace/.stignore") {
		t.Fatalf("expected .stignore path in script, got %q", rec.script)
	}
	if !strings.Contains(rec.script, ".git/\nnode_modules/\n") {
		t.Fatalf("expected ignore content in script, got %q", rec.script)
	}
	if strings.Contains(rec.script, "OKDEV_STIGNORE_EOF") {
		t.Fatalf("did not expect heredoc sentinel in script, got %q", rec.script)
	}
}

func TestWriteRemoteSTIgnoreInPodRejectsInvalidRemotePath(t *testing.T) {
	rec := &syncthingExecRecorder{}
	err := writeRemoteSTIgnoreInPod(context.Background(), rec, "ns", "pod", "/", []string{".git/"})
	if err == nil {
		t.Fatal("expected invalid remote path error")
	}
}

func TestAllocateLocalSyncthingAPIEndpoint(t *testing.T) {
	guiAddr, apiBase, err := allocateLocalSyncthingAPIEndpoint()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(guiAddr, "127.0.0.1:") {
		t.Fatalf("unexpected guiAddr %q", guiAddr)
	}
	if apiBase != "http://"+guiAddr {
		t.Fatalf("unexpected apiBase %q", apiBase)
	}
}

func TestApplyManagedSyncthingFolderDefaults(t *testing.T) {
	folder := map[string]any{
		"id": "okdev-test",
	}

	applyManagedSyncthingFolderDefaults(folder, 300, 0, false)

	if got := folder["fsWatcherEnabled"]; got != true {
		t.Fatalf("expected fsWatcherEnabled=true, got %#v", got)
	}
	if got := folder["fsWatcherDelayS"]; got != syncthingWatcherDelayS {
		t.Fatalf("expected fsWatcherDelayS=%d, got %#v", syncthingWatcherDelayS, got)
	}
	if got := folder["rescanIntervalS"]; got != 300 {
		t.Fatalf("expected rescanIntervalS=%d, got %#v", 300, got)
	}
	if got := folder["maxConflicts"]; got != 0 {
		t.Fatalf("expected maxConflicts=0, got %#v", got)
	}
	if got := folder["ignoreDelete"]; got != false {
		t.Fatalf("expected ignoreDelete=false, got %#v", got)
	}
}

func TestApplyManagedSyncthingFolderDefaultsCustomWatcherDelay(t *testing.T) {
	folder := map[string]any{
		"id": "okdev-test",
	}

	applyManagedSyncthingFolderDefaults(folder, 300, 5, false)

	if got := folder["fsWatcherDelayS"]; got != 5 {
		t.Fatalf("expected fsWatcherDelayS=5, got %#v", got)
	}
}

func TestApplyManagedSyncthingFolderDefaultsIgnoreDelete(t *testing.T) {
	folder := map[string]any{
		"id": "okdev-test",
	}

	applyManagedSyncthingFolderDefaults(folder, 300, 0, true)

	if got := folder["ignoreDelete"]; got != true {
		t.Fatalf("expected ignoreDelete=true, got %#v", got)
	}
}

func TestSyncthingOverrideFolder(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/db/override") {
			if r.URL.Query().Get("folder") != "okdev-test" {
				t.Errorf("expected folder=okdev-test, got %s", r.URL.Query().Get("folder"))
			}
			called = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := syncthingOverrideFolder(context.Background(), srv.URL, "k", "okdev-test"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected /rest/db/override to be called")
	}
}

func TestSyncthingRestart(t *testing.T) {
	var called bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/rest/system/restart" {
			called = true
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if err := syncthingRestart(context.Background(), srv.URL, "k"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("expected /rest/system/restart to be called")
	}
}

func TestRunTwoPhaseInitialSync(t *testing.T) {
	// Track API calls to verify the two-phase protocol.
	var (
		mu             sync.Mutex
		configPuts     int
		overrideCalls  int
		restartCalls   int
		needBytesValue int64 = 1024 // starts non-zero, switches to 0 after override
	)

	localCfg := map[string]any{
		"devices": []any{},
		"folders": []any{},
	}
	remoteCfg := map[string]any{
		"devices": []any{},
		"folders": []any{},
	}

	handler := func(cfg *map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
				b, _ := json.Marshal(*cfg)
				w.Write(b)
			case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
				body, _ := io.ReadAll(r.Body)
				var newCfg map[string]any
				json.Unmarshal(body, &newCfg)
				*cfg = newCfg
				configPuts++
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
				w.Write([]byte(`{"myID":"DEVICE-ID"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/connections":
				w.Write([]byte(`{"connections":{"REMOTE":{"connected":true},"LOCAL":{"connected":true}}}`))
			case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/db/override"):
				overrideCalls++
				// Simulate: after override, remote becomes in sync.
				needBytesValue = 0
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/db/status":
				fmt.Fprintf(w, `{"needBytes":%d}`, needBytesValue)
			case r.Method == http.MethodPost && r.URL.Path == "/rest/system/restart":
				restartCalls++
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}

	localSrv := httptest.NewServer(handler(&localCfg))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(handler(&remoteCfg))
	defer remoteSrv.Close()

	var buf bytes.Buffer
	ctx := context.Background()
	sc := syncthingSyncConfig{
		rescanInterval: 300,
		watcherDelay:   1,
		relays:         false,
		compression:    false,
	}

	err := runTwoPhaseInitialSync(ctx, &buf, localSrv.URL, "k", "LOCAL", remoteSrv.URL, "k", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "/tmp/remote", sc)
	if err != nil {
		t.Fatal(err)
	}

	// Verify override was called at least twice (phase 1 + phase 1b).
	if overrideCalls < 2 {
		t.Fatalf("expected /rest/db/override to be called at least twice (phase 1 + 1b), got %d", overrideCalls)
	}

	// Verify both instances were restarted for phase 2.
	if restartCalls != 2 {
		t.Fatalf("expected 2 restart calls (local+remote), got %d", restartCalls)
	}

	// Verify config was written multiple times (phase 1 + phase 2 for both sides).
	// Phase 1 (2) + phase 1b (1) + phase 2 (2) = 5 config puts minimum.
	if configPuts < 5 {
		t.Fatalf("expected at least 5 config puts (including phase 1b deletion propagation), got %d", configPuts)
	}

	// Verify final local folder type is sendreceive.
	folders, _ := syncthingObjectArray(localCfg, "folders")
	if len(folders) == 0 {
		t.Fatal("expected at least one folder in local config")
	}
	fm, _ := syncthingObjectMap(folders[0], "folders")
	if got := asString(fm["type"]); got != "sendreceive" {
		t.Fatalf("expected local folder type sendreceive after phase 2, got %s", got)
	}
	if got, ok := fm["ignoreDelete"].(bool); !ok || got {
		t.Fatalf("expected ignoreDelete=false after phase 2, got %#v", fm["ignoreDelete"])
	}

	// Verify output mentions all phases.
	out := buf.String()
	if !strings.Contains(out, "Phase 1:") {
		t.Fatal("expected output to mention Phase 1")
	}
	if !strings.Contains(out, "Phase 1b:") {
		t.Fatal("expected output to mention Phase 1b (deletion propagation)")
	}
	if !strings.Contains(out, "Phase 2:") {
		t.Fatal("expected output to mention Phase 2")
	}
}

func TestRunTwoPhaseInitialSyncWaitsForDeletionConvergence(t *testing.T) {
	// Simulate: phase 1 completes quickly, but phase 1b (deletion propagation)
	// takes multiple ticks before the remote converges.
	var (
		mu                sync.Mutex
		overrideCalls     int
		phase1bNeedCalls  int
		phase1bConvergeAt = 3 // converge after 3 needBytes polls in phase 1b
		inPhase1b         bool
	)

	localCfg := map[string]any{"devices": []any{}, "folders": []any{}}
	remoteCfg := map[string]any{"devices": []any{}, "folders": []any{}}

	handler := func(cfg *map[string]any, isLocal bool) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
				b, _ := json.Marshal(*cfg)
				w.Write(b)
			case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
				body, _ := io.ReadAll(r.Body)
				var newCfg map[string]any
				json.Unmarshal(body, &newCfg)
				*cfg = newCfg
				// Detect phase 1b: local config with sendonly + ignoreDelete=false.
				if isLocal {
					if folders, ok := newCfg["folders"].([]any); ok && len(folders) > 0 {
						if fm, ok := folders[0].(map[string]any); ok {
							if asString(fm["type"]) == "sendonly" {
								if id, ok := fm["ignoreDelete"].(bool); ok && !id {
									inPhase1b = true
								}
							}
						}
					}
				}
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
				w.Write([]byte(`{"myID":"DEVICE-ID"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/connections":
				w.Write([]byte(`{"connections":{"REMOTE":{"connected":true},"LOCAL":{"connected":true}}}`))
			case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/db/override"):
				overrideCalls++
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/db/status":
				need := int64(0)
				if inPhase1b {
					phase1bNeedCalls++
					if phase1bNeedCalls < phase1bConvergeAt {
						need = 512 // still converging
					}
					// else need = 0, converged
				}
				fmt.Fprintf(w, `{"needBytes":%d}`, need)
			case r.Method == http.MethodPost && r.URL.Path == "/rest/system/restart":
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}

	localSrv := httptest.NewServer(handler(&localCfg, true))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(handler(&remoteCfg, false))
	defer remoteSrv.Close()

	var buf bytes.Buffer
	sc := syncthingSyncConfig{rescanInterval: 300, watcherDelay: 1}

	err := runTwoPhaseInitialSync(context.Background(), &buf, localSrv.URL, "k", "LOCAL", remoteSrv.URL, "k", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "/tmp/remote", sc)
	if err != nil {
		t.Fatal(err)
	}

	// Phase 1b must have polled multiple times before converging.
	if phase1bNeedCalls < phase1bConvergeAt {
		t.Fatalf("expected at least %d phase 1b needBytes polls, got %d", phase1bConvergeAt, phase1bNeedCalls)
	}

	// Output should show non-zero needBytes during phase 1b.
	out := buf.String()
	if !strings.Contains(out, "deletion propagation: remote needBytes=512") {
		t.Fatalf("expected phase 1b progress with needBytes=512, got: %s", out)
	}
}

func TestRunTwoPhaseInitialSyncWaitsForPeerConnection(t *testing.T) {
	var (
		mu                sync.Mutex
		overrideCalls     int
		connectionPolls   int
		connectionReadyAt = 3
	)

	localCfg := map[string]any{"devices": []any{}, "folders": []any{}}
	remoteCfg := map[string]any{"devices": []any{}, "folders": []any{}}

	handler := func(cfg *map[string]any) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			mu.Lock()
			defer mu.Unlock()

			switch {
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
				_ = json.NewEncoder(w).Encode(*cfg)
			case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
				body, _ := io.ReadAll(r.Body)
				var newCfg map[string]any
				_ = json.Unmarshal(body, &newCfg)
				*cfg = newCfg
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/status":
				w.Write([]byte(`{"myID":"DEVICE-ID"}`))
			case r.Method == http.MethodGet && r.URL.Path == "/rest/system/connections":
				connectionPolls++
				connected := connectionPolls >= connectionReadyAt
				fmt.Fprintf(w, `{"connections":{"REMOTE":{"connected":%t},"LOCAL":{"connected":%t}}}`, connected, connected)
			case r.Method == http.MethodPost && strings.HasPrefix(r.URL.Path, "/rest/db/override"):
				overrideCalls++
				w.WriteHeader(http.StatusOK)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/db/status":
				w.Write([]byte(`{"needBytes":0}`))
			case r.Method == http.MethodPost && r.URL.Path == "/rest/system/restart":
				w.WriteHeader(http.StatusOK)
			default:
				w.WriteHeader(http.StatusNotFound)
			}
		}
	}

	localSrv := httptest.NewServer(handler(&localCfg))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(handler(&remoteCfg))
	defer remoteSrv.Close()

	var buf bytes.Buffer
	sc := syncthingSyncConfig{rescanInterval: 300, watcherDelay: 1}
	err := runTwoPhaseInitialSync(context.Background(), &buf, localSrv.URL, "k", "LOCAL", remoteSrv.URL, "k", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "/tmp/remote", sc)
	if err != nil {
		t.Fatal(err)
	}

	if connectionPolls < connectionReadyAt {
		t.Fatalf("expected at least %d connection polls, got %d", connectionReadyAt, connectionPolls)
	}
	if overrideCalls == 0 {
		t.Fatal("expected override calls during startup")
	}
	if !strings.Contains(buf.String(), "peer connection: local=false remote=false") {
		t.Fatalf("expected connection progress output, got: %s", buf.String())
	}
}

func TestApplyManagedSyncthingGlobalDefaults(t *testing.T) {
	cfg := map[string]any{
		"options": map[string]any{
			"autoUpgradeIntervalH": float64(12),
			"upgradeToPreReleases": true,
			"urAccepted":           float64(2),
			"relaysEnabled":        true,
		},
	}

	applyManagedSyncthingGlobalDefaults(cfg, false)

	options, err := syncthingObjectMap(cfg["options"], "options")
	if err != nil {
		t.Fatal(err)
	}
	if got := options["autoUpgradeIntervalH"]; got != 0 {
		t.Fatalf("expected autoUpgradeIntervalH=0, got %#v", got)
	}
	if got := options["upgradeToPreReleases"]; got != false {
		t.Fatalf("expected upgradeToPreReleases=false, got %#v", got)
	}
	if got := options["urAccepted"]; got != -1 {
		t.Fatalf("expected urAccepted=-1, got %#v", got)
	}
	if got := options["relaysEnabled"]; got != false {
		t.Fatalf("expected relaysEnabled=false, got %#v", got)
	}
	if got := options["globalAnnounceEnabled"]; got != false {
		t.Fatalf("expected globalAnnounceEnabled=false, got %#v", got)
	}
	if got := options["localAnnounceEnabled"]; got != false {
		t.Fatalf("expected localAnnounceEnabled=false, got %#v", got)
	}
	if got := options["natEnabled"]; got != false {
		t.Fatalf("expected natEnabled=false, got %#v", got)
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

func TestLocalSyncthingLogPathRejectsEmptyHome(t *testing.T) {
	if _, err := localSyncthingLogPath("   "); err == nil {
		t.Fatal("expected empty home error")
	}
}

func TestOpenLocalSyncthingLogRotatesOversizedFile(t *testing.T) {
	home := t.TempDir()
	logPath, err := localSyncthingLogPath(home)
	if err != nil {
		t.Fatalf("localSyncthingLogPath: %v", err)
	}
	if err := os.WriteFile(logPath, bytes.Repeat([]byte("x"), 11*1024*1024), 0o644); err != nil {
		t.Fatalf("seed oversized log: %v", err)
	}

	f, err := openLocalSyncthingLog(logPath)
	if err != nil {
		t.Fatalf("openLocalSyncthingLog: %v", err)
	}
	_ = f.Close()

	if _, err := os.Stat(logPath + ".1"); err != nil {
		t.Fatalf("expected rotated backup log: %v", err)
	}
	info, err := os.Stat(logPath)
	if err != nil {
		t.Fatalf("stat active log: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected empty active log after rotation, got %d", info.Size())
	}
}

func TestNewLocalSyncthingServeCommandDisablesAutoUpgrade(t *testing.T) {
	cmd := newLocalSyncthingServeCommand("/tmp/syncthing", "/tmp/home", "127.0.0.1:8384")
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--no-upgrade") {
		t.Fatalf("expected --no-upgrade in args, got %q", args)
	}
	env := strings.Join(cmd.Env, "\n")
	if !strings.Contains(env, "STNOUPGRADE=1") {
		t.Fatalf("expected STNOUPGRADE=1 in env, got %q", env)
	}
}

func TestFolderTypesForMode(t *testing.T) {
	tests := []struct {
		mode       string
		wantLocal  string
		wantRemote string
	}{
		{mode: "up", wantLocal: "sendonly", wantRemote: "receiveonly"},
		{mode: "down", wantLocal: "receiveonly", wantRemote: "sendonly"},
		{mode: "bi", wantLocal: "sendreceive", wantRemote: "sendreceive"},
		{mode: "other", wantLocal: "sendreceive", wantRemote: "sendreceive"},
	}
	for _, tc := range tests {
		t.Run(tc.mode, func(t *testing.T) {
			gotLocal, gotRemote := folderTypesForMode(tc.mode)
			if gotLocal != tc.wantLocal || gotRemote != tc.wantRemote {
				t.Fatalf("unexpected folder types local=%q remote=%q", gotLocal, gotRemote)
			}
		})
	}
}

func TestSyncthingDeviceIDRejectsEmptyID(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"myID":""}`))
	}))
	defer srv.Close()

	if _, err := syncthingDeviceID(context.Background(), srv.URL, "k"); err == nil {
		t.Fatal("expected empty myID error")
	}
}

func TestWaitSyncthingAPISucceedsAfterRetry(t *testing.T) {
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		if calls == 1 {
			http.Error(w, "not ready", http.StatusServiceUnavailable)
			return
		}
		_, _ = w.Write([]byte(`{"myID":"LOCALID"}`))
	}))
	defer srv.Close()

	if err := waitSyncthingAPI(context.Background(), srv.URL, "k", 1500*time.Millisecond); err != nil {
		t.Fatal(err)
	}
}

func TestSyncthingAPIRequestWithContextSetsHeaders(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("X-API-Key"); got != "secret" {
			t.Fatalf("unexpected API key header %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type %q", got)
		}
		_, _ = w.Write([]byte(`ok`))
	}))
	defer srv.Close()

	body, err := syncthingAPIRequestWithContext(context.Background(), http.MethodPost, srv.URL, "secret", "/rest/config", []byte(`{}`), "application/json")
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != "ok" {
		t.Fatalf("unexpected body %q", body)
	}
}

func TestSyncthingAPIRequestWithContextRejectsErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "bad", http.StatusBadRequest)
	}))
	defer srv.Close()

	if _, err := syncthingAPIRequestWithContext(context.Background(), http.MethodGet, srv.URL, "k", "/rest/config", nil, ""); err == nil {
		t.Fatal("expected status error")
	}
}

func TestConfigureSyncthingPeerAddsAndUpdatesConfig(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{},
		"folders": []any{},
	}
	var putBodies [][]byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
			_ = json.NewEncoder(w).Encode(cfg)
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			putBodies = append(putBodies, body)
			if err := json.Unmarshal(body, &cfg); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "sendreceive", 300, 0, false, false, false); err != nil {
		t.Fatal(err)
	}
	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22001", "okdev-test", "/tmp/updated", "sendonly", 120, 0, false, false, false); err != nil {
		t.Fatal(err)
	}
	if len(putBodies) != 2 {
		t.Fatalf("expected 2 config writes, got %d", len(putBodies))
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	device, err := syncthingObjectMap(devices[0], "devices")
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(device["deviceID"]); got != "REMOTE" {
		t.Fatalf("unexpected deviceID %q", got)
	}
	addresses, ok := device["addresses"].([]any)
	if !ok || len(addresses) != 1 || addresses[0] != "tcp://127.0.0.1:22001" {
		t.Fatalf("unexpected device addresses %#v", device["addresses"])
	}
	if got := asString(device["compression"]); got != "metadata" {
		t.Fatalf("expected compression=metadata, got %q", got)
	}

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("expected 1 folder, got %d", len(folders))
	}
	folder, err := syncthingObjectMap(folders[0], "folders")
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(folder["path"]); got != "/tmp/updated" {
		t.Fatalf("unexpected folder path %q", got)
	}
	if got := asString(folder["type"]); got != "sendonly" {
		t.Fatalf("unexpected folder type %q", got)
	}
	if got := folder["rescanIntervalS"]; got != float64(120) {
		t.Fatalf("unexpected rescanIntervalS %#v", got)
	}
	if got := folder["fsWatcherEnabled"]; got != true {
		t.Fatalf("unexpected fsWatcherEnabled %#v", got)
	}
	if got := folder["maxConflicts"]; got != float64(0) {
		t.Fatalf("expected maxConflicts=0, got %#v", got)
	}
	if got := folder["ignoreDelete"]; got != false {
		t.Fatalf("expected ignoreDelete=false, got %#v", got)
	}
	options, err := syncthingObjectMap(cfg["options"], "options")
	if err != nil {
		t.Fatal(err)
	}
	if got := options["autoUpgradeIntervalH"]; got != float64(0) {
		t.Fatalf("unexpected autoUpgradeIntervalH %#v", got)
	}
	if got := options["upgradeToPreReleases"]; got != false {
		t.Fatalf("unexpected upgradeToPreReleases %#v", got)
	}
	if got := options["urAccepted"]; got != float64(-1) {
		t.Fatalf("unexpected urAccepted %#v", got)
	}
	if got := options["relaysEnabled"]; got != false {
		t.Fatalf("unexpected relaysEnabled %#v", got)
	}
	if got := options["globalAnnounceEnabled"]; got != false {
		t.Fatalf("unexpected globalAnnounceEnabled %#v", got)
	}
	if got := options["localAnnounceEnabled"]; got != false {
		t.Fatalf("unexpected localAnnounceEnabled %#v", got)
	}
	if got := options["natEnabled"]; got != false {
		t.Fatalf("unexpected natEnabled %#v", got)
	}
}

func TestEffectiveLocalExcludes(t *testing.T) {
	got := effectiveLocalExcludes(nil)
	if len(got) != len(defaultSyncExcludes) {
		t.Fatalf("expected defaults, got %v", got)
	}
	for i, v := range defaultSyncExcludes {
		if got[i] != v {
			t.Fatalf("expected %q at index %d, got %q", v, i, got[i])
		}
	}
	custom := []string{".idea", "build"}
	got = effectiveLocalExcludes(custom)
	if len(got) != 2 || got[0] != ".idea" || got[1] != "build" {
		t.Fatalf("expected custom excludes, got %v", got)
	}
}

func TestWriteSTIgnoreDefaultExcludes(t *testing.T) {
	dir := t.TempDir()
	if err := writeSTIgnore(dir, effectiveLocalExcludes(nil)); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, ".stignore"))
	if err != nil {
		t.Fatal(err)
	}
	for _, pattern := range defaultSyncExcludes {
		if !strings.Contains(string(got), pattern) {
			t.Fatalf("expected default exclude %q in stignore, got %q", pattern, got)
		}
	}
}

func TestWriteLocalSTIgnorePropagatesStatError(t *testing.T) {
	base := t.TempDir()
	filePath := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSTIgnore(filePath, nil); err == nil {
		t.Fatal("expected stat error for invalid local path")
	}
}

func TestConfigureSyncthingPeerCompressionAlways(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{},
		"folders": []any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
			_ = json.NewEncoder(w).Encode(cfg)
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &cfg); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "sendreceive", 300, 0, false, false, true); err != nil {
		t.Fatal(err)
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	device, err := syncthingObjectMap(devices[0], "devices")
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(device["compression"]); got != "always" {
		t.Fatalf("expected compression=always, got %q", got)
	}
}

func TestConfigureSyncthingPeerUsesDynamicAddressWhenPeerAddrEmpty(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{},
		"folders": []any{},
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
			_ = json.NewEncoder(w).Encode(cfg)
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
			body, err := io.ReadAll(r.Body)
			if err != nil {
				t.Fatal(err)
			}
			if err := json.Unmarshal(body, &cfg); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "REMOTE", "LOCAL", "", "okdev-test", "/tmp/remote", "receiveonly", 300, 0, false, false, false); err != nil {
		t.Fatal(err)
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 1 {
		t.Fatalf("expected 1 device, got %d", len(devices))
	}
	device, err := syncthingObjectMap(devices[0], "devices")
	if err != nil {
		t.Fatal(err)
	}
	addresses, ok := device["addresses"].([]any)
	if !ok || len(addresses) != 1 || addresses[0] != "dynamic" {
		t.Fatalf("expected dynamic device address, got %#v", device["addresses"])
	}
}

func TestSyncthingServeHomePatternEscapesRegexMeta(t *testing.T) {
	home := `/tmp/okdev/sync.[test]+(demo)?`
	got := syncthingServeHomePattern(home)
	want := `serve --home /tmp/okdev/sync\.\[test\]\+\(demo\)\?`
	if got != want {
		t.Fatalf("syncthingServeHomePattern() = %q, want %q", got, want)
	}
}

func TestReadLocalSyncthingEndpoint(t *testing.T) {
	home := t.TempDir()
	configXML := `<configuration>
  <gui>
    <apikey>test-key-123</apikey>
    <address>127.0.0.1:8384</address>
  </gui>
</configuration>`
	if err := os.WriteFile(filepath.Join(home, "config.xml"), []byte(configXML), 0o644); err != nil {
		t.Fatal(err)
	}

	apiBase, apiKey, err := readLocalSyncthingEndpoint(home)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "test-key-123" {
		t.Fatalf("expected apiKey=test-key-123, got %s", apiKey)
	}
	if apiBase != "http://127.0.0.1:8384" {
		t.Fatalf("expected apiBase=http://127.0.0.1:8384, got %s", apiBase)
	}
}

func TestReadLocalSyncthingEndpointPrefersRuntimeEndpointFile(t *testing.T) {
	home := t.TempDir()
	configXML := `<configuration>
  <gui>
    <apikey>test-key-123</apikey>
    <address>127.0.0.1:8384</address>
  </gui>
</configuration>`
	if err := os.WriteFile(filepath.Join(home, "config.xml"), []byte(configXML), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSyncthingEndpoint(home, "http://127.0.0.1:49200"); err != nil {
		t.Fatal(err)
	}

	apiBase, apiKey, err := readLocalSyncthingEndpoint(home)
	if err != nil {
		t.Fatal(err)
	}
	if apiKey != "test-key-123" {
		t.Fatalf("expected apiKey=test-key-123, got %s", apiKey)
	}
	if apiBase != "http://127.0.0.1:49200" {
		t.Fatalf("expected apiBase=http://127.0.0.1:49200, got %s", apiBase)
	}
}

func TestReadLocalSyncthingEndpointMissing(t *testing.T) {
	home := t.TempDir()
	_, _, err := readLocalSyncthingEndpoint(home)
	if err == nil {
		t.Fatal("expected error for missing config")
	}
}

func TestWarnSyncHealthStopped(t *testing.T) {
	// Use a non-existent session name so the PID file won't exist.
	var buf bytes.Buffer
	status := warnSyncHealth(&buf, "nonexistent-session-for-test")
	if status != syncHealthStopped {
		t.Fatalf("expected stopped, got %s", status)
	}
	if !strings.Contains(buf.String(), "sync is not running") {
		t.Fatalf("expected warning message, got: %s", buf.String())
	}
}

type fakeExecClient struct {
	scripts []string
}

func (f *fakeExecClient) ExecShInContainer(_ context.Context, _, _, _, script string) ([]byte, error) {
	f.scripts = append(f.scripts, script)
	return nil, nil
}

func TestResetRemoteWorkspace(t *testing.T) {
	client := &fakeExecClient{}
	ctx := context.Background()

	t.Run("basic reset without preserve", func(t *testing.T) {
		client.scripts = nil
		if err := resetRemoteWorkspace(ctx, client, "ns", "pod", "/workspace", nil); err != nil {
			t.Fatal(err)
		}
		if len(client.scripts) != 1 {
			t.Fatalf("expected 1 exec call, got %d", len(client.scripts))
		}
		script := client.scripts[0]
		if !strings.Contains(script, "find") || !strings.Contains(script, "-mindepth 1 -maxdepth 1") {
			t.Fatalf("expected find command in script, got: %s", script)
		}
		if strings.Contains(script, "mkdir -p") {
			t.Fatalf("should not create preserve dir without preserve paths")
		}
	})

	t.Run("reset with preserve paths", func(t *testing.T) {
		client.scripts = nil
		if err := resetRemoteWorkspace(ctx, client, "ns", "pod", "/workspace", []string{".venv", ".cache"}); err != nil {
			t.Fatal(err)
		}
		script := client.scripts[0]
		if !strings.Contains(script, ".okdev-reset-preserve") {
			t.Fatalf("expected preserve dir reference, got: %s", script)
		}
		if !strings.Contains(script, ".venv") {
			t.Fatalf("expected .venv in script, got: %s", script)
		}
		if !strings.Contains(script, ".cache") {
			t.Fatalf("expected .cache in script, got: %s", script)
		}
		// Verify both move (preserve) and restore phases exist.
		venvCount := strings.Count(script, ".venv")
		if venvCount < 4 {
			t.Fatalf("expected .venv referenced at least 4 times (preserve check+move, restore check+move), got %d in: %s", venvCount, script)
		}
		// Verify cleanup.
		if !strings.Contains(script, "rm -rf") || !strings.Contains(script, ".okdev-reset-preserve") {
			t.Fatalf("expected preserve dir cleanup, got: %s", script)
		}
	})

	t.Run("rejects root path", func(t *testing.T) {
		if err := resetRemoteWorkspace(ctx, client, "ns", "pod", "/", nil); err == nil {
			t.Fatal("expected error for root path")
		}
	})

	t.Run("rejects path traversal in preservePaths", func(t *testing.T) {
		for _, p := range []string{"../../etc", "../data", "foo/../../bar", "/absolute/path"} {
			if err := resetRemoteWorkspace(ctx, client, "ns", "pod", "/workspace", []string{p}); err == nil {
				t.Fatalf("expected error for preservePath %q", p)
			}
		}
	})

	t.Run("allows nested preserve paths", func(t *testing.T) {
		client.scripts = nil
		if err := resetRemoteWorkspace(ctx, client, "ns", "pod", "/workspace", []string{"data/cache", ".local/share"}); err != nil {
			t.Fatal(err)
		}
		if len(client.scripts) != 1 {
			t.Fatalf("expected 1 exec call, got %d", len(client.scripts))
		}
	})
}
