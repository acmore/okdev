package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
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

func TestSyncthingFolderScannedIdle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"idle","globalFiles":3}`))
	}))
	defer srv.Close()
	scanned, err := syncthingFolderScanned(context.Background(), srv.URL, "k", "okdev-test")
	if err != nil {
		t.Fatal(err)
	}
	if !scanned {
		t.Fatal("expected idle folder to be considered scanned")
	}
}

func TestSyncthingFolderScannedScanning(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"scanning","globalFiles":0}`))
	}))
	defer srv.Close()
	scanned, err := syncthingFolderScanned(context.Background(), srv.URL, "k", "okdev-test")
	if err != nil {
		t.Fatal(err)
	}
	if scanned {
		t.Fatal("expected scanning folder to NOT be considered scanned")
	}
}

func TestSyncthingFolderScannedEmpty(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"","globalFiles":0}`))
	}))
	defer srv.Close()
	scanned, err := syncthingFolderScanned(context.Background(), srv.URL, "k", "okdev-test")
	if err != nil {
		t.Fatal(err)
	}
	if scanned {
		t.Fatal("expected empty-state folder to NOT be considered scanned")
	}
}

func TestWaitForLocalFolderScanCompletes(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c <= 2 {
			_, _ = w.Write([]byte(`{"state":"scanning","globalFiles":0}`))
		} else {
			_, _ = w.Write([]byte(`{"state":"idle","globalFiles":1}`))
		}
	}))
	defer srv.Close()
	globalFiles, err := waitForLocalFolderScan(context.Background(), srv.URL, "k", "okdev-test", false, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if globalFiles != 1 {
		t.Fatalf("expected globalFiles=1, got %d", globalFiles)
	}
	mu.Lock()
	if calls < 3 {
		t.Fatalf("expected at least 3 calls, got %d", calls)
	}
	mu.Unlock()
}

func TestWaitForLocalFolderScanTimeout(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"scanning","globalFiles":0}`))
	}))
	defer srv.Close()
	_, err := waitForLocalFolderScan(context.Background(), srv.URL, "k", "okdev-test", false, 500*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestWaitForLocalFolderScanWaitsForExpectedFiles(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		calls++
		c := calls
		mu.Unlock()
		if c <= 2 {
			_, _ = w.Write([]byte(`{"state":"idle","globalFiles":0}`))
		} else {
			_, _ = w.Write([]byte(`{"state":"idle","globalFiles":1}`))
		}
	}))
	defer srv.Close()
	globalFiles, err := waitForLocalFolderScan(context.Background(), srv.URL, "k", "okdev-test", true, 5*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if globalFiles != 1 {
		t.Fatalf("expected globalFiles=1, got %d", globalFiles)
	}
	mu.Lock()
	if calls < 3 {
		t.Fatalf("expected at least 3 calls, got %d", calls)
	}
	mu.Unlock()
}

func TestSyncthingInitialSyncComplete(t *testing.T) {
	if !syncthingInitialSyncComplete(100, 0, 99.9, 0) {
		t.Fatal("expected zero local and remote bytes to be treated as complete")
	}
	if syncthingInitialSyncComplete(100, 1, 100, 0) {
		t.Fatal("expected local pending bytes to block completion")
	}
	if syncthingInitialSyncComplete(100, 0, 100, 1) {
		t.Fatal("expected remote pending bytes to block completion")
	}
	if syncthingInitialSyncComplete(99.8, 0, 100, 0) {
		t.Fatal("expected sub-threshold local completion percentage to block completion")
	}
}

func TestSyncthingBootstrapInitialSyncComplete(t *testing.T) {
	if !syncthingBootstrapInitialSyncComplete(100, 0) {
		t.Fatal("expected bootstrap sync with zero local pending bytes to be complete")
	}
	if syncthingBootstrapInitialSyncComplete(99.8, 0) {
		t.Fatal("expected bootstrap sync to require near-complete local percentage")
	}
	if syncthingBootstrapInitialSyncComplete(100, 1) {
		t.Fatal("expected bootstrap sync to block on local pending bytes")
	}
}

func TestTwoPhaseInitialSyncReadyRequiresConnectedPeersForEmptyFolders(t *testing.T) {
	if twoPhaseInitialSyncReady(100, 0, 0, true, false, false) {
		t.Fatal("did not expect two-phase initial sync ready while peers are disconnected")
	}
	if !twoPhaseInitialSyncReady(100, 0, 0, true, true, true) {
		t.Fatal("expected two-phase initial sync ready once peers are connected")
	}
}

func TestRemoteIndexExpectationForLocal(t *testing.T) {
	t.Run("steady state requires current count", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(12, 12)
		if exp.expectedFiles != 12 || exp.requireExactMatch {
			t.Fatalf("unexpected expectation %+v", exp)
		}
		if !exp.needsRemotePoll() {
			t.Fatal("expected remote poll required when files are present")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 12, LocalFiles: 12}) {
			t.Fatal("expected matching remote counts to satisfy steady-state expectation")
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 11, LocalFiles: 12}) {
			t.Fatal("expected lagging remote global to fail expectation")
		}
	})

	t.Run("growth uses current count, not baseline", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(12, 20)
		if exp.expectedFiles != 20 {
			t.Fatalf("expected expectation to track grown count, got %+v", exp)
		}
		if exp.requireExactMatch {
			t.Fatalf("growth must not require exact match (remote may be ahead transiently), got %+v", exp)
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 12, LocalFiles: 12}) {
			t.Fatal("expected stale remote at old baseline to fail when local has grown")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 20, LocalFiles: 20}) {
			t.Fatal("expected remote caught up to grown count to satisfy expectation")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 25, LocalFiles: 25}) {
			t.Fatal("expected remote ahead of local to satisfy expectation in growth case")
		}
	})

	t.Run("growth from empty baseline requires remote catch-up", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(0, 8)
		if exp.expectedFiles != 8 || exp.requireExactMatch {
			t.Fatalf("unexpected expectation %+v", exp)
		}
		if !exp.needsRemotePoll() {
			t.Fatal("expected remote poll required when local has grown from zero baseline")
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 0, LocalFiles: 0}) {
			t.Fatal("expected empty remote to fail when local has grown")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 8, LocalFiles: 8}) {
			t.Fatal("expected matched remote to satisfy expectation")
		}
	})

	t.Run("partial shrink requires exact match", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(12, 5)
		if exp.expectedFiles != 5 || !exp.requireExactMatch {
			t.Fatalf("expected exact-match requirement on partial shrink, got %+v", exp)
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 12, LocalFiles: 12}) {
			t.Fatal("expected stale remote (still at pre-shrink baseline) to fail exact-match gate")
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 7, LocalFiles: 5}) {
			t.Fatal("expected partially-applied remote shrink to fail exact-match gate")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 5, LocalFiles: 5}) {
			t.Fatal("expected fully converged remote to satisfy exact-match gate")
		}
	})

	t.Run("shrink to zero from positive baseline requires exact convergence", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(12, 0)
		if exp.expectedFiles != 0 || !exp.requireExactMatch {
			t.Fatalf("expected exact-match requirement when local drops to zero, got %+v", exp)
		}
		if !exp.needsRemotePoll() {
			t.Fatal("expected remote poll required to verify convergence")
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 12, LocalFiles: 12}) {
			t.Fatal("expected stale remote index to fail convergence requirement")
		}
		if exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 0, LocalFiles: 1}) {
			t.Fatal("expected remote local-files lag to fail convergence requirement")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{GlobalFiles: 0, LocalFiles: 0}) {
			t.Fatal("expected fully converged remote to satisfy convergence requirement")
		}
	})

	t.Run("baseline zero with no growth skips remote poll", func(t *testing.T) {
		exp := remoteIndexExpectationForLocal(0, 0)
		if exp.expectedFiles != 0 || exp.requireExactMatch {
			t.Fatalf("unexpected expectation %+v", exp)
		}
		if exp.needsRemotePoll() {
			t.Fatal("expected no remote poll when both baseline and current are zero")
		}
		if !exp.satisfiedBy(syncthingFolderStatusInfo{}) {
			t.Fatal("expected empty-baseline expectation to be trivially satisfied")
		}
	})
}

// fakeFolderStatusServer simulates a syncthing /rest/db/status endpoint
// whose reported counts can be reconfigured between calls. It serves both
// the "local" and "remote" halves of an evaluateRemoteIndexReceived call.
type fakeFolderStatusServer struct {
	mu                sync.Mutex
	localGlobalFiles  int64
	remoteGlobalFiles int64
	remoteLocalFiles  int64
	localCalls        int
	remoteCalls       int
}

func (s *fakeFolderStatusServer) handler(role string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/db/status" {
			http.Error(w, "unexpected path", http.StatusNotFound)
			return
		}
		s.mu.Lock()
		defer s.mu.Unlock()
		var global, local int64
		if role == "local" {
			s.localCalls++
			global = s.localGlobalFiles
			local = s.localGlobalFiles
		} else {
			s.remoteCalls++
			global = s.remoteGlobalFiles
			local = s.remoteLocalFiles
		}
		_, _ = fmt.Fprintf(w, `{"state":"idle","globalFiles":%d,"localFiles":%d,"needBytes":0}`, global, local)
	})
}

func TestEvaluateRemoteIndexReceivedHandlesShrink(t *testing.T) {
	state := &fakeFolderStatusServer{}
	localSrv := httptest.NewServer(state.handler("local"))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(state.handler("remote"))
	defer remoteSrv.Close()

	t.Run("blocks readiness when remote still has pre-shrink index", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 0
		state.remoteGlobalFiles = 12
		state.remoteLocalFiles = 12
		state.mu.Unlock()

		ok, exp, remote := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if ok {
			t.Fatalf("expected stale remote index to block readiness, got expectation=%+v remote=%+v", exp, remote)
		}
		if !exp.requireExactMatch {
			t.Fatalf("expected exact-match requirement when local shrunk, got %+v", exp)
		}
	})

	t.Run("returns ready once remote drops to zero", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 0
		state.remoteGlobalFiles = 0
		state.remoteLocalFiles = 0
		state.mu.Unlock()

		ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if !ok {
			t.Fatalf("expected converged remote to be ready, got expectation=%+v", exp)
		}
	})

	t.Run("partial shrink blocks while remote still reports pre-shrink count", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 5
		state.remoteGlobalFiles = 12
		state.remoteLocalFiles = 12
		state.mu.Unlock()

		ok, _, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if ok {
			t.Fatal("expected partial shrink to block readiness while remote index is stale")
		}
	})

	t.Run("partial shrink unblocks once remote matches reduced count", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 5
		state.remoteGlobalFiles = 5
		state.remoteLocalFiles = 5
		state.mu.Unlock()

		ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if !ok {
			t.Fatalf("expected matching remote count to be ready after partial shrink, got %+v", exp)
		}
		if exp.expectedFiles != 5 || !exp.requireExactMatch {
			t.Fatalf("expected expectation to follow current count with exact-match, got %+v", exp)
		}
	})
}

func TestEvaluateRemoteIndexReceivedHandlesGrowth(t *testing.T) {
	state := &fakeFolderStatusServer{}
	localSrv := httptest.NewServer(state.handler("local"))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(state.handler("remote"))
	defer remoteSrv.Close()

	t.Run("blocks readiness when remote still at pre-growth baseline", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 20
		state.remoteGlobalFiles = 12
		state.remoteLocalFiles = 12
		state.mu.Unlock()

		ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if ok {
			t.Fatalf("expected remote at stale baseline to block readiness after local growth, got %+v", exp)
		}
		if exp.expectedFiles != 20 {
			t.Fatalf("expected expectation to use current count after growth, got %+v", exp)
		}
	})

	t.Run("ready once remote catches up to grown local count", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 20
		state.remoteGlobalFiles = 20
		state.remoteLocalFiles = 20
		state.mu.Unlock()

		ok, _, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
		if !ok {
			t.Fatal("expected remote caught up to grown local count to be ready")
		}
	})

	t.Run("growth from empty baseline waits for remote", func(t *testing.T) {
		state.mu.Lock()
		state.localGlobalFiles = 8
		state.remoteGlobalFiles = 0
		state.remoteLocalFiles = 0
		state.mu.Unlock()

		ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 0)
		if ok {
			t.Fatalf("expected empty remote to block readiness when local grew from zero baseline, got %+v", exp)
		}
		if exp.expectedFiles != 8 {
			t.Fatalf("expected expectation to follow current count from zero baseline, got %+v", exp)
		}
	})
}

func TestEvaluateRemoteIndexReceivedSkipsRemoteWhenBothCountsZero(t *testing.T) {
	var localCalls, remoteCalls int
	var mu sync.Mutex
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		localCalls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"idle","globalFiles":0,"localFiles":0,"needBytes":0}`))
	}))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remoteCalls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"idle","globalFiles":99,"localFiles":99,"needBytes":0}`))
	}))
	defer remoteSrv.Close()

	ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 0)
	if !ok {
		t.Fatalf("expected baseline-zero expectation to be trivially ready, got %+v", exp)
	}
	mu.Lock()
	defer mu.Unlock()
	if localCalls != 1 {
		t.Fatalf("expected exactly one local poll, got %d", localCalls)
	}
	if remoteCalls != 0 {
		t.Fatalf("expected remote poll to be skipped, got %d", remoteCalls)
	}
}

func TestEvaluateRemoteIndexReceivedBlocksOnLocalPollError(t *testing.T) {
	var remoteCalls int
	var mu sync.Mutex
	localSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer localSrv.Close()
	remoteSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		remoteCalls++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"state":"idle","globalFiles":12,"localFiles":12,"needBytes":0}`))
	}))
	defer remoteSrv.Close()

	ok, exp, _ := evaluateRemoteIndexReceived(context.Background(), localSrv.URL, "k", remoteSrv.URL, "k", "okdev-test", 12)
	if ok {
		t.Fatalf("expected local poll failure to block readiness rather than fall back to stale baseline, got %+v", exp)
	}
	mu.Lock()
	defer mu.Unlock()
	if remoteCalls != 0 {
		t.Fatalf("expected remote poll to be skipped after local poll failure, got %d", remoteCalls)
	}
}

func TestFormatSyncthingMiB(t *testing.T) {
	if got := formatSyncthingMiB(0); got != "0.0 MiB" {
		t.Fatalf("unexpected zero format %q", got)
	}
	if got := formatSyncthingMiB(1572864); got != "1.5 MiB" {
		t.Fatalf("unexpected MiB format %q", got)
	}
}

func TestFormatSyncthingSpeed(t *testing.T) {
	if got := formatSyncthingSpeed(0); got != "0.0 MiB/s" {
		t.Fatalf("unexpected zero speed format %q", got)
	}
	if got := formatSyncthingSpeed(1572864); got != "1.5 MiB/s" {
		t.Fatalf("unexpected speed format %q", got)
	}
}

func TestSyncthingManagedFolderType(t *testing.T) {
	cfg := map[string]any{
		"folders": []any{
			map[string]any{"id": "okdev-a", "type": "receiveonly"},
			map[string]any{"id": "okdev-b", "type": "sendreceive"},
		},
	}
	got, found, err := syncthingManagedFolderType(cfg, "okdev-b")
	if err != nil {
		t.Fatal(err)
	}
	if !found || got != "sendreceive" {
		t.Fatalf("unexpected folder lookup found=%v type=%q", found, got)
	}
}

func TestShouldResumeTwoPhaseBootstrap(t *testing.T) {
	if !shouldResumeTwoPhaseBootstrap("", false) {
		t.Fatal("expected missing managed folder to resume two-phase")
	}
	if !shouldResumeTwoPhaseBootstrap("receiveonly", true) {
		t.Fatal("expected receiveonly folder to resume two-phase")
	}
	if shouldResumeTwoPhaseBootstrap("sendreceive", true) {
		t.Fatal("expected sendreceive folder to skip two-phase resume")
	}
}

func TestSyncthingPeerConnectionTotals(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/system/connections" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"connections":{"REMOTEID":{"inBytesTotal":2048,"outBytesTotal":4096}}}`))
	}))
	defer srv.Close()

	stats, err := syncthingPeerConnectionTotals(context.Background(), srv.URL, "k", "REMOTEID")
	if err != nil {
		t.Fatal(err)
	}
	if stats.InBytesTotal != 2048 || stats.OutBytesTotal != 4096 {
		t.Fatalf("unexpected stats %+v", stats)
	}
}

func TestSyncthingRateEstimator(t *testing.T) {
	var est syncthingRateEstimator
	start := time.Unix(100, 0)
	if _, ok := est.Estimate(start, 1024); ok {
		t.Fatal("expected first sample to have no rate")
	}
	got, ok := est.Estimate(start.Add(2*time.Second), 3*1024*1024)
	if !ok {
		t.Fatal("expected rate after second sample")
	}
	if got <= 0 {
		t.Fatalf("expected positive rate, got %d", got)
	}
}

func TestFormatInitialSyncProgressDetail(t *testing.T) {
	got := formatInitialSyncProgressDetail(syncthingInitialSyncProgress{
		Mode:            "bi",
		LocalNeedBytes:  10 * 1024 * 1024,
		RemoteNeedBytes: 5 * 1024 * 1024,
		UploadBps:       2 * 1024 * 1024,
		DownloadBps:     512 * 1024,
		HasUploadBps:    true,
		HasDownloadBps:  true,
	})
	for _, want := range []string{"15.0 MiB remaining", "local->remote 10.0 MiB", "remote->local 5.0 MiB", "up 2.0 MiB/s", "down 0.5 MiB/s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
	if strings.Contains(got, "eta") {
		t.Fatalf("did not expect ETA in %q", got)
	}
}

func TestFormatInitialSyncProgressDetailTwoPhase(t *testing.T) {
	got := formatInitialSyncProgressDetail(syncthingInitialSyncProgress{
		Mode:            "two-phase",
		LocalNeedBytes:  10 * 1024 * 1024,
		RemoteNeedBytes: 5 * 1024 * 1024,
		UploadBps:       3 * 1024 * 1024,
		HasUploadBps:    true,
	})
	for _, want := range []string{"10.0 MiB remaining to remote", "3.0 MiB/s"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in %q", want, got)
		}
	}
	if strings.Contains(got, "remote->local") {
		t.Fatalf("did not expect remote->local detail in two-phase output: %q", got)
	}
	if strings.Contains(got, "eta") {
		t.Fatalf("did not expect ETA in %q", got)
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
	if err := writeLocalSTIgnore(dir); err != nil {
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
	if err := writeLocalSTIgnore(dir); err != nil {
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

func TestStopLocalSyncthingForHomeStopsRecordedPID(t *testing.T) {
	home := t.TempDir()
	cmd := exec.Command("sleep", "30")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
	})
	if err := writeLocalSyncthingPID(home, cmd.Process.Pid); err != nil {
		t.Fatal(err)
	}
	if err := stopLocalSyncthingForHome(home); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case <-time.After(2 * time.Second):
		t.Fatal("expected recorded process to exit")
	case <-done:
	}
	pidPath, err := localSyncthingPIDPath(home)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("expected pid file to be removed, got err=%v", err)
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

func TestParseLocalSyncthingAPIBaseUsesLatestLoopbackAddress(t *testing.T) {
	logText := strings.Join([]string{
		"INFO: GUI and API listening on 127.0.0.1:49100",
		"INFO: GUI and API listening on 127.0.0.1:49101",
	}, "\n")

	base, ok := parseLocalSyncthingAPIBase(logText)
	if !ok {
		t.Fatal("expected API base")
	}
	if base != "http://127.0.0.1:49101" {
		t.Fatalf("unexpected API base %q", base)
	}
}

func TestParseLocalSyncthingAPIBaseRejectsNonLoopbackAddress(t *testing.T) {
	_, ok := parseLocalSyncthingAPIBase("INFO: GUI and API listening on 10.0.0.12:49100")
	if ok {
		t.Fatal("did not expect non-loopback API base")
	}
}

func TestReadFileFromOffsetIgnoresStaleSyncthingLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "local.log")
	stale := "INFO: GUI and API listening on 127.0.0.1:49100\n"
	fresh := "INFO: GUI and API listening on 127.0.0.1:49101\n"
	if err := os.WriteFile(logPath, []byte(stale+fresh), 0o644); err != nil {
		t.Fatal(err)
	}

	logText := readFileFromOffset(logPath, int64(len(stale)), 8192)
	base, ok := parseLocalSyncthingAPIBase(logText)
	if !ok {
		t.Fatal("expected API base")
	}
	if base != "http://127.0.0.1:49101" {
		t.Fatalf("unexpected API base %q", base)
	}
}

func TestWaitLocalSyncthingAPIEndpointUsesCurrentLogAndAPIKey(t *testing.T) {
	const apiKey = "session-key"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/rest/system/status" {
			http.NotFound(w, r)
			return
		}
		if got := r.Header.Get("X-API-Key"); got != apiKey {
			http.Error(w, "bad key", http.StatusForbidden)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"myID":"LOCAL"}`)
	}))
	defer srv.Close()

	logPath := filepath.Join(t.TempDir(), "local.log")
	stale := "INFO: GUI and API listening on 127.0.0.1:49100\n"
	fresh := "INFO: GUI and API listening on " + strings.TrimPrefix(srv.URL, "http://") + "\n"
	if err := os.WriteFile(logPath, []byte(stale+fresh), 0o644); err != nil {
		t.Fatal(err)
	}

	proc := localSyncthingProcess{PID: 1234, LogPath: logPath, LogOffset: int64(len(stale))}
	base, err := waitLocalSyncthingAPIEndpoint(context.Background(), proc, apiKey, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if base != srv.URL {
		t.Fatalf("expected %q, got %q", srv.URL, base)
	}
}

func TestLockLocalSyncthingHomeHonorsContext(t *testing.T) {
	home := t.TempDir()
	first, err := lockLocalSyncthingHome(context.Background(), home)
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	second, err := lockLocalSyncthingHome(ctx, home)
	if err == nil {
		second.Close()
		t.Fatal("expected second lock attempt to time out")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", err)
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
	if got := folder["filesystemType"]; got != "basic" {
		t.Fatalf("expected filesystemType=basic, got %#v", got)
	}
	if got := folder["order"]; got != "random" {
		t.Fatalf("expected order=random, got %#v", got)
	}
	if got := folder["scanProgressIntervalS"]; got != 1 {
		t.Fatalf("expected scanProgressIntervalS=1, got %#v", got)
	}
	if got := folder["disableSparseFiles"]; got != false {
		t.Fatalf("expected disableSparseFiles=false, got %#v", got)
	}
	if got := folder["disableTempIndexes"]; got != false {
		t.Fatalf("expected disableTempIndexes=false, got %#v", got)
	}
	if got := folder["useLargeBlocks"]; got != false {
		t.Fatalf("expected useLargeBlocks=false, got %#v", got)
	}
	if got := folder["copyRangeMethod"]; got != "all" {
		t.Fatalf("expected copyRangeMethod=all, got %#v", got)
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

func TestSyncthingRestartRequired(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/rest/config/restart-required" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_, _ = w.Write([]byte(`{"requiresRestart":true}`))
	}))
	defer srv.Close()

	required, err := syncthingRestartRequired(context.Background(), srv.URL, "k")
	if err != nil {
		t.Fatal(err)
	}
	if !required {
		t.Fatal("expected restart-required=true")
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
				fmt.Fprintf(w, `{"needBytes":%d,"state":"idle","globalFiles":1,"localFiles":1}`, needBytesValue)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config/restart-required":
				w.Write([]byte(`{"requiresRestart":false}`))
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
	// Verify neither instance reported restart-required in phase 2.
	if restartCalls != 0 {
		t.Fatalf("expected 0 restart calls when restart-required=false, got %d", restartCalls)
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
				fmt.Fprintf(w, `{"needBytes":%d,"state":"idle","globalFiles":1,"localFiles":1}`, need)
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config/restart-required":
				w.Write([]byte(`{"requiresRestart":false}`))
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
	if !strings.Contains(out, "deletion propagation: 0.0 MiB remaining") {
		t.Fatalf("expected simplified phase 1b progress output, got: %s", out)
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
				w.Write([]byte(`{"needBytes":0,"state":"idle","globalFiles":1,"localFiles":1}`))
			case r.Method == http.MethodGet && r.URL.Path == "/rest/config/restart-required":
				w.Write([]byte(`{"requiresRestart":false}`))
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
	if !strings.Contains(buf.String(), "waiting for peer connection") {
		t.Fatalf("expected waiting-for-peer output, got: %s", buf.String())
	}
}

func TestRunSyncthingSyncStopsOwnedLocalDaemonWhenTwoPhaseBootstrapFails(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origEnsureBinary := syncthingEnsureBinaryFn
	origResolveTargetRef := resolveTargetRefFn
	origExecInSyncthingContainer := execInSyncthingContainerFn
	origStartPortForward := startSyncthingPortForwardFn
	origReadRemoteKey := readRemoteSyncthingAPIKeyFn
	origStartLocal := startAndWaitForLocalSyncthingFn
	origWaitAPI := waitSyncthingAPIFn
	origDeviceID := syncthingDeviceIDFn
	origRunTwoPhase := runTwoPhaseInitialSyncFn
	origStopOwned := stopOwnedLocalSyncthingFn
	t.Cleanup(func() {
		syncthingEnsureBinaryFn = origEnsureBinary
		resolveTargetRefFn = origResolveTargetRef
		execInSyncthingContainerFn = origExecInSyncthingContainer
		startSyncthingPortForwardFn = origStartPortForward
		readRemoteSyncthingAPIKeyFn = origReadRemoteKey
		startAndWaitForLocalSyncthingFn = origStartLocal
		waitSyncthingAPIFn = origWaitAPI
		syncthingDeviceIDFn = origDeviceID
		runTwoPhaseInitialSyncFn = origRunTwoPhase
		stopOwnedLocalSyncthingFn = origStopOwned
	})

	localDir := filepath.Join(home, "repo")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}

	syncthingEnsureBinaryFn = func(context.Context, string, bool) (string, error) {
		return "/tmp/syncthing", nil
	}
	resolveTargetRefFn = func(context.Context, *Options, *config.DevEnvironment, string, string, targetResolverClient) (workload.TargetRef, error) {
		return workload.TargetRef{PodName: "pod-0"}, nil
	}
	execInSyncthingContainerFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string, string) ([]byte, error) {
		return nil, nil
	}
	startSyncthingPortForwardFn = func(context.Context, *Options, string, string) (context.CancelFunc, string, string, error) {
		return func() {}, "http://remote", "tcp://127.0.0.1:22000", nil
	}
	readRemoteSyncthingAPIKeyFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string) (string, error) {
		return "remote-key", nil
	}
	startAndWaitForLocalSyncthingFn = func(context.Context, string, string) (string, string, int, error) {
		return "http://local", "local-key", 4321, nil
	}
	waitSyncthingAPIFn = func(context.Context, string, string, time.Duration) error {
		return nil
	}
	syncthingDeviceIDFn = func(_ context.Context, base, _ string) (string, error) {
		if base == "http://local" {
			return "LOCAL", nil
		}
		return "REMOTE", nil
	}
	runTwoPhaseInitialSyncFn = func(context.Context, io.Writer, string, string, string, string, string, string, string, string, string, string, syncthingSyncConfig) error {
		return errors.New("bootstrap timeout")
	}

	var (
		stopCalls int
		stopHome  string
		stopPID   int
	)
	stopOwnedLocalSyncthingFn = func(home string, pid int) error {
		stopCalls++
		stopHome = home
		stopPID = pid
		return nil
	}

	cfg := &config.DevEnvironment{}
	cfg.SetDefaults()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	err := runSyncthingSync(cmd, &Options{}, cfg, "default", "sess-cleanup", "two-phase", []syncengine.Pair{{
		Local:  localDir,
		Remote: "/workspace",
	}}, nil)
	if err == nil || !strings.Contains(err.Error(), "two-phase initial sync") {
		t.Fatalf("expected two-phase initial sync error, got %v", err)
	}
	if stopCalls != 1 {
		t.Fatalf("expected local syncthing cleanup after bootstrap failure, got %d calls", stopCalls)
	}
	if stopPID != 4321 {
		t.Fatalf("unexpected stopped pid: got %d want %d", stopPID, 4321)
	}
	wantHome, err := localSyncthingHome("sess-cleanup")
	if err != nil {
		t.Fatalf("localSyncthingHome: %v", err)
	}
	if stopHome != wantHome {
		t.Fatalf("unexpected stopped home: got %q want %q", stopHome, wantHome)
	}
}

func TestRunSyncthingSyncMarksReadyBeforeTwoPhaseBootstrap(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origEnsureBinary := syncthingEnsureBinaryFn
	origResolveTargetRef := resolveTargetRefFn
	origExecInSyncthingContainer := execInSyncthingContainerFn
	origStartPortForward := startSyncthingPortForwardFn
	origReadRemoteKey := readRemoteSyncthingAPIKeyFn
	origStartLocal := startAndWaitForLocalSyncthingFn
	origWaitAPI := waitSyncthingAPIFn
	origDeviceID := syncthingDeviceIDFn
	origMarkReady := markSyncthingReadyFn
	origRunTwoPhase := runTwoPhaseInitialSyncFn
	origSaveConfigHash := saveSyncthingConfigHashFn
	origStopOwned := stopOwnedLocalSyncthingFn
	origRunHealthLoop := runSyncHealthLoopFn
	t.Cleanup(func() {
		syncthingEnsureBinaryFn = origEnsureBinary
		resolveTargetRefFn = origResolveTargetRef
		execInSyncthingContainerFn = origExecInSyncthingContainer
		startSyncthingPortForwardFn = origStartPortForward
		readRemoteSyncthingAPIKeyFn = origReadRemoteKey
		startAndWaitForLocalSyncthingFn = origStartLocal
		waitSyncthingAPIFn = origWaitAPI
		syncthingDeviceIDFn = origDeviceID
		markSyncthingReadyFn = origMarkReady
		runTwoPhaseInitialSyncFn = origRunTwoPhase
		saveSyncthingConfigHashFn = origSaveConfigHash
		stopOwnedLocalSyncthingFn = origStopOwned
		runSyncHealthLoopFn = origRunHealthLoop
	})

	localDir := filepath.Join(home, "repo")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}

	syncthingEnsureBinaryFn = func(context.Context, string, bool) (string, error) {
		return "/tmp/syncthing", nil
	}
	resolveTargetRefFn = func(context.Context, *Options, *config.DevEnvironment, string, string, targetResolverClient) (workload.TargetRef, error) {
		return workload.TargetRef{PodName: "pod-0"}, nil
	}
	execInSyncthingContainerFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string, string) ([]byte, error) {
		return nil, nil
	}
	startSyncthingPortForwardFn = func(context.Context, *Options, string, string) (context.CancelFunc, string, string, error) {
		return func() {}, "http://remote", "tcp://127.0.0.1:22000", nil
	}
	readRemoteSyncthingAPIKeyFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string) (string, error) {
		return "remote-key", nil
	}
	startAndWaitForLocalSyncthingFn = func(context.Context, string, string) (string, string, int, error) {
		return "http://local", "local-key", 4321, nil
	}
	waitSyncthingAPIFn = func(context.Context, string, string, time.Duration) error {
		return nil
	}
	syncthingDeviceIDFn = func(_ context.Context, base, _ string) (string, error) {
		if base == "http://local" {
			return "LOCAL", nil
		}
		return "REMOTE", nil
	}

	var calls []string
	markSyncthingReadyFn = func(string) error {
		calls = append(calls, "mark-ready")
		return nil
	}
	runTwoPhaseInitialSyncFn = func(context.Context, io.Writer, string, string, string, string, string, string, string, string, string, string, syncthingSyncConfig) error {
		calls = append(calls, "two-phase")
		return nil
	}
	saveSyncthingConfigHashFn = func(string, string) error { return nil }
	stopOwnedLocalSyncthingFn = func(string, int) error { return nil }
	runSyncHealthLoopFn = func(<-chan os.Signal, io.Writer, syncHealthChecker) {}

	cfg := &config.DevEnvironment{}
	cfg.SetDefaults()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := runSyncthingSync(cmd, &Options{}, cfg, "default", "sess-ready", "two-phase", []syncengine.Pair{{
		Local:  localDir,
		Remote: "/workspace",
	}}, nil); err != nil {
		t.Fatalf("runSyncthingSync: %v", err)
	}

	if got, want := calls, []string{"mark-ready", "two-phase"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected call order: got %v want %v", got, want)
	}
}

func TestRunSyncthingSyncMarksBootstrapCompleteBeforeHealthLoop(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	origEnsureBinary := syncthingEnsureBinaryFn
	origResolveTargetRef := resolveTargetRefFn
	origExecInSyncthingContainer := execInSyncthingContainerFn
	origStartPortForward := startSyncthingPortForwardFn
	origReadRemoteKey := readRemoteSyncthingAPIKeyFn
	origStartLocal := startAndWaitForLocalSyncthingFn
	origWaitAPI := waitSyncthingAPIFn
	origDeviceID := syncthingDeviceIDFn
	origRunTwoPhase := runTwoPhaseInitialSyncFn
	origSaveConfigHash := saveSyncthingConfigHashFn
	origStopOwned := stopOwnedLocalSyncthingFn
	origRunHealthLoop := runSyncHealthLoopFn
	t.Cleanup(func() {
		syncthingEnsureBinaryFn = origEnsureBinary
		resolveTargetRefFn = origResolveTargetRef
		execInSyncthingContainerFn = origExecInSyncthingContainer
		startSyncthingPortForwardFn = origStartPortForward
		readRemoteSyncthingAPIKeyFn = origReadRemoteKey
		startAndWaitForLocalSyncthingFn = origStartLocal
		waitSyncthingAPIFn = origWaitAPI
		syncthingDeviceIDFn = origDeviceID
		runTwoPhaseInitialSyncFn = origRunTwoPhase
		saveSyncthingConfigHashFn = origSaveConfigHash
		stopOwnedLocalSyncthingFn = origStopOwned
		runSyncHealthLoopFn = origRunHealthLoop
	})

	localDir := filepath.Join(home, "repo")
	if err := os.MkdirAll(localDir, 0o755); err != nil {
		t.Fatalf("mkdir local dir: %v", err)
	}

	syncthingEnsureBinaryFn = func(context.Context, string, bool) (string, error) {
		return "/tmp/syncthing", nil
	}
	resolveTargetRefFn = func(context.Context, *Options, *config.DevEnvironment, string, string, targetResolverClient) (workload.TargetRef, error) {
		return workload.TargetRef{PodName: "pod-0"}, nil
	}
	execInSyncthingContainerFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string, string) ([]byte, error) {
		return nil, nil
	}
	startSyncthingPortForwardFn = func(context.Context, *Options, string, string) (context.CancelFunc, string, string, error) {
		return func() {}, "http://remote", "tcp://127.0.0.1:22000", nil
	}
	readRemoteSyncthingAPIKeyFn = func(context.Context, interface {
		ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	}, string, string) (string, error) {
		return "remote-key", nil
	}
	startAndWaitForLocalSyncthingFn = func(context.Context, string, string) (string, string, int, error) {
		return "http://local", "local-key", 4321, nil
	}
	waitSyncthingAPIFn = func(context.Context, string, string, time.Duration) error {
		return nil
	}
	syncthingDeviceIDFn = func(_ context.Context, base, _ string) (string, error) {
		if base == "http://local" {
			return "LOCAL", nil
		}
		return "REMOTE", nil
	}

	completePath, err := syncthingBootstrapCompletePath("sess-handoff")
	if err != nil {
		t.Fatalf("syncthingBootstrapCompletePath: %v", err)
	}

	runTwoPhaseInitialSyncFn = func(context.Context, io.Writer, string, string, string, string, string, string, string, string, string, string, syncthingSyncConfig) error {
		if _, err := os.Stat(completePath); !os.IsNotExist(err) {
			t.Fatalf("bootstrap completion marker should not exist before two-phase finishes, got err=%v", err)
		}
		return nil
	}
	saveSyncthingConfigHashFn = func(string, string) error { return nil }
	stopOwnedLocalSyncthingFn = func(string, int) error { return nil }
	runSyncHealthLoopFn = func(<-chan os.Signal, io.Writer, syncHealthChecker) {
		if _, err := os.Stat(completePath); err != nil {
			t.Fatalf("expected bootstrap completion marker before health loop, got %v", err)
		}
	}

	cfg := &config.DevEnvironment{}
	cfg.SetDefaults()
	cmd := &cobra.Command{}
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)

	if err := runSyncthingSync(cmd, &Options{}, cfg, "default", "sess-handoff", "two-phase", []syncengine.Pair{{
		Local:  localDir,
		Remote: "/workspace",
	}}, nil); err != nil {
		t.Fatalf("runSyncthingSync: %v", err)
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
	if got := options["reconnectionIntervalS"]; got != 1 {
		t.Fatalf("expected reconnectionIntervalS=1, got %#v", got)
	}
	if got := options["progressUpdateIntervalS"]; got != 1 {
		t.Fatalf("expected progressUpdateIntervalS=1, got %#v", got)
	}
	if got := options["setLowPriority"]; got != false {
		t.Fatalf("expected setLowPriority=false, got %#v", got)
	}
	if got := options["keepTemporariesH"]; got != 24 {
		t.Fatalf("expected keepTemporariesH=24, got %#v", got)
	}
	if got := options["tempIndexMinBlocks"]; got != 10 {
		t.Fatalf("expected tempIndexMinBlocks=10, got %#v", got)
	}
}

func TestApplyManagedSyncthingDeviceDefaults(t *testing.T) {
	device := map[string]any{"deviceID": "REMOTE"}

	applyManagedSyncthingDeviceDefaults(device)

	if got := device["paused"]; got != false {
		t.Fatalf("expected paused=false, got %#v", got)
	}
	if got := device["autoAcceptFolders"]; got != false {
		t.Fatalf("expected autoAcceptFolders=false, got %#v", got)
	}
	if got := device["maxSendKbps"]; got != 0 {
		t.Fatalf("expected maxSendKbps=0, got %#v", got)
	}
	if got := device["maxRecvKbps"]; got != 0 {
		t.Fatalf("expected maxRecvKbps=0, got %#v", got)
	}
	if got := device["maxRequestKiB"]; got != 0 {
		t.Fatalf("expected maxRequestKiB=0, got %#v", got)
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

func TestNewLocalSyncthingServeCommandSupportsEphemeralGUIAddress(t *testing.T) {
	cmd := newLocalSyncthingServeCommand("/tmp/syncthing", "/tmp/home", localSyncthingGUIAddr)
	args := strings.Join(cmd.Args, " ")
	if !strings.Contains(args, "--gui-address=http://127.0.0.1:0") {
		t.Fatalf("expected ephemeral loopback GUI address in args, got %q", args)
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

func TestSyncthingSetIgnoresPostsPatterns(t *testing.T) {
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Fatalf("unexpected method %s", r.Method)
		}
		if r.URL.Path != "/rest/db/ignores" {
			t.Fatalf("unexpected path %s", r.URL.Path)
		}
		if got := r.URL.Query().Get("folder"); got != "okdev-test" {
			t.Fatalf("unexpected folder query %q", got)
		}
		if got := r.Header.Get("X-API-Key"); got != "secret" {
			t.Fatalf("unexpected API key %q", got)
		}
		if got := r.Header.Get("Content-Type"); got != "application/json" {
			t.Fatalf("unexpected content type %q", got)
		}
		var err error
		gotBody, err = io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		_, _ = w.Write([]byte(`{"ignore":["profiles/","*.prof"]}`))
	}))
	defer srv.Close()

	if err := syncthingSetIgnores(context.Background(), srv.URL, "secret", "okdev-test", []string{"profiles/", "*.prof"}); err != nil {
		t.Fatal(err)
	}
	if string(gotBody) != `{"ignore":["profiles/","*.prof"]}` {
		t.Fatalf("unexpected request body %s", gotBody)
	}
}

func TestConfigureSyncthingPeerAddsAndUpdatesConfig(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{},
		"folders": []any{
			map[string]any{
				"id":         "default",
				"label":      "Default Folder",
				"path":       "/Users/test/Sync",
				"markerName": ".stfolder",
			},
		},
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
	if got := device["autoAcceptFolders"]; got != false {
		t.Fatalf("expected autoAcceptFolders=false, got %#v", got)
	}
	if got := device["maxRequestKiB"]; got != float64(0) {
		t.Fatalf("expected maxRequestKiB=0, got %#v", got)
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
	if got := asString(folder["id"]); got != "okdev-test" {
		t.Fatalf("unexpected folder id %q", got)
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
	if got := folder["filesystemType"]; got != "basic" {
		t.Fatalf("expected filesystemType=basic, got %#v", got)
	}
	if got := folder["scanProgressIntervalS"]; got != float64(1) {
		t.Fatalf("expected scanProgressIntervalS=1, got %#v", got)
	}
	if got := folder["disableTempIndexes"]; got != false {
		t.Fatalf("expected disableTempIndexes=false, got %#v", got)
	}
	if got := folder["useLargeBlocks"]; got != false {
		t.Fatalf("expected useLargeBlocks=false, got %#v", got)
	}
	if got := folder["copyRangeMethod"]; got != "all" {
		t.Fatalf("expected copyRangeMethod=all, got %#v", got)
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
	if got := options["reconnectionIntervalS"]; got != float64(1) {
		t.Fatalf("unexpected reconnectionIntervalS %#v", got)
	}
	if got := options["progressUpdateIntervalS"]; got != float64(1) {
		t.Fatalf("unexpected progressUpdateIntervalS %#v", got)
	}
	if got := options["setLowPriority"]; got != false {
		t.Fatalf("unexpected setLowPriority %#v", got)
	}
}

func TestConfigureSyncthingPeerPrunesStaleManagedPeerDevices(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{
			map[string]any{
				"deviceID":  "STALE-1",
				"name":      "okdev-peer",
				"addresses": []any{"tcp://127.0.0.1:22001"},
			},
			map[string]any{
				"deviceID":  "KEEP-MANUAL",
				"name":      "teammate-laptop",
				"addresses": []any{"dynamic"},
			},
			map[string]any{
				"deviceID":  "STALE-2",
				"name":      "okdev-peer",
				"addresses": []any{"tcp://127.0.0.1:22002"},
			},
		},
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

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "LOCAL", "REMOTE", "tcp://127.0.0.1:22000", "okdev-test", "/tmp/local", "sendreceive", 300, 0, false, false, false); err != nil {
		t.Fatal(err)
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		t.Fatal(err)
	}
	if len(devices) != 2 {
		t.Fatalf("expected current managed peer plus manual device, got %d devices", len(devices))
	}

	gotIDs := make([]string, 0, len(devices))
	for _, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, asString(m["deviceID"]))
	}
	wantIDs := []string{"KEEP-MANUAL", "REMOTE"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("unexpected device IDs %v, want %v", gotIDs, wantIDs)
	}
}

func TestConfigureSyncthingPeerPreservesMeshReceiverFolderPeers(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{
			map[string]any{
				"deviceID":  "LOCAL",
				"name":      managedSyncthingPeer,
				"addresses": []any{"dynamic"},
			},
			map[string]any{
				"deviceID":  "RECV",
				"name":      "okdev-mesh-receiver",
				"addresses": []any{"tcp://10.0.0.2:22000"},
			},
		},
		"folders": []any{
			map[string]any{
				"id":   "okdev-test",
				"path": "/workspace",
				"type": "sendreceive",
				"devices": []any{
					map[string]any{"deviceID": "REMOTE"},
					map[string]any{"deviceID": "LOCAL"},
					map[string]any{"deviceID": "RECV"},
				},
			},
		},
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

	if err := configureSyncthingPeer(context.Background(), srv.URL, "k", "REMOTE", "LOCAL", "", "okdev-test", "/workspace", "sendreceive", 300, 1, false, false, false); err != nil {
		t.Fatal(err)
	}

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		t.Fatal(err)
	}
	folder, err := syncthingObjectMap(folders[0], "folders")
	if err != nil {
		t.Fatal(err)
	}
	folderDevices, ok := folder["devices"].([]any)
	if !ok {
		t.Fatalf("expected folder devices array, got %#v", folder["devices"])
	}
	gotIDs := make([]string, 0, len(folderDevices))
	for _, d := range folderDevices {
		m, err := syncthingObjectMap(d, "folder devices")
		if err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, asString(m["deviceID"]))
	}
	wantIDs := []string{"REMOTE", "LOCAL", "RECV"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("unexpected folder device IDs %v, want %v", gotIDs, wantIDs)
	}
}

func TestSyncthingBootstrapAndRuntimeContexts(t *testing.T) {
	parent := context.Background()
	bootstrapCtx, cancel, runtimeCtx := syncthingBootstrapAndRuntimeContexts(parent)
	t.Cleanup(cancel)

	if runtimeCtx != parent {
		t.Fatal("expected runtime context to be the parent command context")
	}

	if _, ok := runtimeCtx.Deadline(); ok {
		t.Fatal("expected runtime context to avoid the bootstrap deadline")
	}

	deadline, ok := bootstrapCtx.Deadline()
	if !ok {
		t.Fatal("expected bootstrap context to have a deadline")
	}
	if remaining := time.Until(deadline); remaining <= 0 || remaining > syncthingBootstrapTimeout {
		t.Fatalf("unexpected bootstrap deadline remaining %s", remaining)
	}
}

func TestConfigureSyncthingMeshHubKeepsAllReceiversInFolder(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{
			map[string]any{
				"deviceID":  "RECV2",
				"addresses": []any{"tcp://old:22000"},
			},
		},
		"folders": []any{
			map[string]any{
				"id":         "default",
				"label":      "Default Folder",
				"path":       "/Users/test/Sync",
				"markerName": ".stfolder",
			},
			map[string]any{
				"id":      "okdev-test",
				"path":    "/tmp/old",
				"type":    "sendonly",
				"devices": []any{map[string]any{"deviceID": "HUB"}, map[string]any{"deviceID": "RECV2"}},
			},
		},
	}
	var putBodies int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/rest/config":
			_ = json.NewEncoder(w).Encode(cfg)
		case r.Method == http.MethodPut && r.URL.Path == "/rest/config":
			putBodies++
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	err := configureSyncthingMeshHub(context.Background(), srv.URL, "k", "HUB", map[string]string{
		"RECV2": "tcp://10.0.0.2:22000",
		"RECV1": "tcp://10.0.0.1:22000",
	}, "okdev-test", "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if putBodies != 1 {
		t.Fatalf("expected 1 config write, got %d", putBodies)
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		t.Fatal(err)
	}
	deviceAddrs := map[string][]any{}
	for _, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			t.Fatal(err)
		}
		id := asString(m["deviceID"])
		addrs, ok := m["addresses"].([]any)
		if !ok {
			t.Fatalf("expected addresses array for %s, got %#v", id, m["addresses"])
		}
		deviceAddrs[id] = addrs
	}
	for id, wantAddr := range map[string]string{
		"RECV1": "tcp://10.0.0.1:22000",
		"RECV2": "tcp://10.0.0.2:22000",
	} {
		addrs := deviceAddrs[id]
		if len(addrs) != 1 || addrs[0] != wantAddr {
			t.Fatalf("unexpected addresses for %s: %#v", id, addrs)
		}
	}

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		t.Fatal(err)
	}
	if len(folders) != 1 {
		t.Fatalf("expected default folder removed, got %d folders", len(folders))
	}
	folder, err := syncthingObjectMap(folders[0], "folders")
	if err != nil {
		t.Fatal(err)
	}
	if got := asString(folder["path"]); got != "/workspace" {
		t.Fatalf("unexpected folder path %q", got)
	}
	if got := asString(folder["type"]); got != "sendreceive" {
		t.Fatalf("unexpected folder type %q", got)
	}
	folderDevices, ok := folder["devices"].([]any)
	if !ok {
		t.Fatalf("expected folder devices array, got %#v", folder["devices"])
	}
	gotIDs := make([]string, 0, len(folderDevices))
	for _, d := range folderDevices {
		m, err := syncthingObjectMap(d, "folder devices")
		if err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, asString(m["deviceID"]))
	}
	wantIDs := []string{"HUB", "RECV1", "RECV2"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("unexpected folder device IDs %v, want %v", gotIDs, wantIDs)
	}
}

func TestWriteSTIgnoreDefaultExcludes(t *testing.T) {
	dir := t.TempDir()
	if err := writeSTIgnore(dir, defaultSyncExcludes); err != nil {
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

func TestConfigureSyncthingMeshHubPreservesExistingNonReceiverFolderPeers(t *testing.T) {
	cfg := map[string]any{
		"devices": []any{
			map[string]any{
				"deviceID":  "HUB",
				"name":      "hub",
				"addresses": []any{"dynamic"},
			},
			map[string]any{
				"deviceID":  "LOCAL",
				"name":      managedSyncthingPeer,
				"addresses": []any{"tcp://127.0.0.1:22000"},
			},
		},
		"folders": []any{
			map[string]any{
				"id":   "okdev-test",
				"path": "/workspace",
				"type": "sendreceive",
				"devices": []any{
					map[string]any{"deviceID": "HUB"},
					map[string]any{"deviceID": "LOCAL"},
				},
			},
		},
		"options": map[string]any{},
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case http.MethodGet + " /rest/config":
			if err := json.NewEncoder(w).Encode(cfg); err != nil {
				t.Fatal(err)
			}
		case http.MethodPut + " /rest/config":
			if err := json.NewDecoder(r.Body).Decode(&cfg); err != nil {
				t.Fatal(err)
			}
			w.WriteHeader(http.StatusOK)
		default:
			t.Fatalf("unexpected request %s %s", r.Method, r.URL.Path)
		}
	}))
	defer srv.Close()

	err := configureSyncthingMeshHub(context.Background(), srv.URL, "k", "HUB", map[string]string{
		"RECV2": "tcp://10.0.0.2:22000",
		"RECV1": "tcp://10.0.0.1:22000",
	}, "okdev-test", "/workspace")
	if err != nil {
		t.Fatal(err)
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
	folderDevices, ok := folder["devices"].([]any)
	if !ok {
		t.Fatalf("expected folder devices array, got %#v", folder["devices"])
	}
	gotIDs := make([]string, 0, len(folderDevices))
	for _, d := range folderDevices {
		m, err := syncthingObjectMap(d, "folder devices")
		if err != nil {
			t.Fatal(err)
		}
		gotIDs = append(gotIDs, asString(m["deviceID"]))
	}
	wantIDs := []string{"HUB", "LOCAL", "RECV1", "RECV2"}
	if strings.Join(gotIDs, ",") != strings.Join(wantIDs, ",") {
		t.Fatalf("unexpected folder device IDs %v, want %v", gotIDs, wantIDs)
	}
}

func TestWriteLocalSTIgnorePropagatesStatError(t *testing.T) {
	base := t.TempDir()
	filePath := filepath.Join(base, "not-a-dir")
	if err := os.WriteFile(filePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := writeLocalSTIgnore(filePath); err == nil {
		t.Fatal("expected stat error for invalid local path")
	}
}

func TestDetectLargeSyncEntriesHonorsSTIgnoreAndReportsDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, ".stignore"), []byte(".git/\nlarge.bin\nignored/\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "ignored"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ignored", "nested.bin"), bytes.Repeat([]byte("a"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".git", "index"), bytes.Repeat([]byte("a"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "large.bin"), bytes.Repeat([]byte("a"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "tracked.bin"), bytes.Repeat([]byte("a"), 2048), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "dataset"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dataset", "part1.bin"), bytes.Repeat([]byte("a"), 700), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "dataset", "part2.bin"), bytes.Repeat([]byte("a"), 700), 0o644); err != nil {
		t.Fatal(err)
	}
	files, dirs, err := detectLargeSyncEntries(dir, 1024, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].Path != "tracked.bin" {
		t.Fatalf("expected only tracked.bin to be reported, got %+v", files)
	}
	if len(dirs) != 1 || dirs[0].Path != "dataset" {
		t.Fatalf("expected dataset dir to be reported, got %+v", dirs)
	}
}

func TestWarnLargeSyncEntriesIncludesFilesAndDirs(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	cacheBlob, err := os.Create(filepath.Join(dir, "cache", "blob.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cacheBlob.Truncate(60 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := cacheBlob.Close(); err != nil {
		t.Fatal(err)
	}
	cacheBlob2, err := os.Create(filepath.Join(dir, "cache", "blob2.bin"))
	if err != nil {
		t.Fatal(err)
	}
	if err := cacheBlob2.Truncate(60 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := cacheBlob2.Close(); err != nil {
		t.Fatal(err)
	}
	weights, err := os.Create(filepath.Join(dir, "weights.pt"))
	if err != nil {
		t.Fatal(err)
	}
	if err := weights.Truncate(101 * 1024 * 1024); err != nil {
		t.Fatal(err)
	}
	if err := weights.Close(); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	warnLargeSyncEntries(&out, dir, 700*1024*1024)
	got := out.String()
	if !strings.Contains(got, "warning: sync still needs") {
		t.Fatalf("expected warning header, got %q", got)
	}
	if !strings.Contains(got, "file 101.0MiB  weights.pt") {
		t.Fatalf("expected large file in warning, got %q", got)
	}
	if !strings.Contains(got, "dir  120.0MiB  cache/") {
		t.Fatalf("expected large dir in warning, got %q", got)
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

func TestStopLocalSyncthingForHomeRunsPatternFallbackAfterRecordedPID(t *testing.T) {
	dir := t.TempDir()
	pkillLog := filepath.Join(dir, "pkill.log")
	pkillPath := filepath.Join(dir, "pkill")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> " + pkillLog + "\nexit 0\n"
	if err := os.WriteFile(pkillPath, []byte(script), 0o755); err != nil {
		t.Fatalf("write pkill stub: %v", err)
	}
	oldPath := os.Getenv("PATH")
	if err := os.Setenv("PATH", dir+string(os.PathListSeparator)+oldPath); err != nil {
		t.Fatalf("set PATH: %v", err)
	}
	t.Cleanup(func() { _ = os.Setenv("PATH", oldPath) })

	home := filepath.Join(dir, "sync-home")
	if err := os.MkdirAll(home, 0o755); err != nil {
		t.Fatalf("mkdir home: %v", err)
	}
	pidPath, err := localSyncthingPIDPath(home)
	if err != nil {
		t.Fatalf("localSyncthingPIDPath: %v", err)
	}
	if err := os.WriteFile(pidPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}

	if err := stopLocalSyncthingForHome(home); err != nil {
		t.Fatalf("stopLocalSyncthingForHome: %v", err)
	}

	if _, err := os.Stat(pidPath); !os.IsNotExist(err) {
		t.Fatalf("expected local pid file to be removed, got err=%v", err)
	}
	got, err := os.ReadFile(pkillLog)
	if err != nil {
		t.Fatalf("read pkill log: %v", err)
	}
	want := "-f " + syncthingServeHomePattern(home)
	if strings.TrimSpace(string(got)) != want {
		t.Fatalf("pkill args = %q, want %q", strings.TrimSpace(string(got)), want)
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

func TestPrepareLocalSyncthingConfigUsesSessionSafeListenAddresses(t *testing.T) {
	home := t.TempDir()
	configXML := `<?xml version="1.0" encoding="UTF-8"?>
<configuration version="37">
    <gui enabled="true" tls="false">
        <address>127.0.0.1:8384</address>
        <apikey>test-key-123</apikey>
    </gui>
    <options>
        <listenAddress>default</listenAddress>
        <globalAnnounceEnabled>true</globalAnnounceEnabled>
    </options>
</configuration>`
	path := filepath.Join(home, "config.xml")
	if err := os.WriteFile(path, []byte(configXML), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := prepareLocalSyncthingConfig(home); err != nil {
		t.Fatalf("prepareLocalSyncthingConfig: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(got)
	if strings.Contains(text, "<listenAddress>default</listenAddress>") {
		t.Fatalf("expected default listen address to be replaced, got:\n%s", text)
	}
	for _, want := range []string{
		"<listenAddress>tcp://127.0.0.1:0</listenAddress>",
		"<listenAddress>quic://127.0.0.1:0</listenAddress>",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q in config, got:\n%s", want, text)
		}
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
