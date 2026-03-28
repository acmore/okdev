package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "sendreceive", 300, false, false); err != nil {
		t.Fatal(err)
	}
	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22001", "okdev-test", "/tmp/updated", "sendonly", 120, false, false); err != nil {
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

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "sendreceive", 300, false, true); err != nil {
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
