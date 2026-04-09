package cli

import (
	"bytes"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

// fakeSyncHealthChecker implements syncHealthChecker for tests.
type fakeSyncHealthChecker struct {
	// connected controls what peerConnected returns on each call.
	// Each call consumes the next value; if exhausted, returns the last value.
	connected []bool
	// connErr controls errors returned by peerConnected. Same indexing as connected.
	connErr []error
	// restoreErr controls errors returned by restorePortForward.
	restoreErr []error

	connectCalls int
	restoreCalls int
}

func (f *fakeSyncHealthChecker) peerConnected() (bool, error) {
	idx := f.connectCalls
	f.connectCalls++
	if idx < len(f.connErr) && f.connErr[idx] != nil {
		return false, f.connErr[idx]
	}
	if idx < len(f.connected) {
		return f.connected[idx], nil
	}
	return f.connected[len(f.connected)-1], nil
}

func (f *fakeSyncHealthChecker) restorePortForward() error {
	idx := f.restoreCalls
	f.restoreCalls++
	if idx < len(f.restoreErr) {
		return f.restoreErr[idx]
	}
	return nil
}

var testSyncHealthConfig = syncHealthLoopConfig{
	interval:      5 * time.Millisecond,
	quickRetries:  5,
	backoffFactor: 2,
	maxInterval:   20 * time.Millisecond,
	maxRetries:    15,
}

func TestSyncHealthLoopExitsOnSignal(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	checker := &fakeSyncHealthChecker{connected: []bool{true}}
	var buf bytes.Buffer

	sigCh <- os.Interrupt
	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)
	// Should return immediately on signal.
}

func TestSyncHealthLoopNoActionWhenConnected(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	checker := &fakeSyncHealthChecker{connected: []bool{true}}
	var buf bytes.Buffer

	go func() {
		time.Sleep(50 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)

	if checker.restoreCalls != 0 {
		t.Fatalf("expected no restore calls when connected, got %d", checker.restoreCalls)
	}
	if buf.Len() != 0 {
		t.Fatalf("expected no output when connected, got %q", buf.String())
	}
}

func TestSyncHealthLoopRestoresOnDisconnect(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	// First check: disconnected → restore succeeds. Second check: connected.
	checker := &fakeSyncHealthChecker{
		connected: []bool{false, true},
	}
	var buf bytes.Buffer

	go func() {
		time.Sleep(50 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)

	if checker.restoreCalls != 1 {
		t.Fatalf("expected 1 restore call, got %d", checker.restoreCalls)
	}
	if !strings.Contains(buf.String(), "reconnected to peer") {
		t.Fatalf("expected reconnection message, got %q", buf.String())
	}
}

func TestSyncHealthLoopSkipsOnAPIError(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	// First call: API error (skip). Second call: disconnected → restore. Third: connected.
	checker := &fakeSyncHealthChecker{
		connected: []bool{false, false, true},
		connErr:   []error{errors.New("connection refused"), nil, nil},
	}
	var buf bytes.Buffer

	go func() {
		time.Sleep(80 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)

	if checker.restoreCalls != 1 {
		t.Fatalf("expected 1 restore call, got %d", checker.restoreCalls)
	}
}

func TestSyncHealthLoopGivesUpAfterMaxRetries(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	cfg := testSyncHealthConfig
	cfg.maxRetries = 3
	cfg.quickRetries = 1

	checker := &fakeSyncHealthChecker{
		connected:  []bool{false},
		restoreErr: []error{errors.New("fail"), errors.New("fail"), errors.New("fail")},
	}
	var buf bytes.Buffer

	go func() {
		time.Sleep(500 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, cfg)

	if checker.restoreCalls != cfg.maxRetries {
		t.Fatalf("expected %d restore calls, got %d", cfg.maxRetries, checker.restoreCalls)
	}
	if !strings.Contains(buf.String(), "giving up") {
		t.Fatalf("expected give-up message, got %q", buf.String())
	}
}

func TestSyncHealthLoopResetsRetriesOnReconnect(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	// Disconnect → restore → connected → disconnect → restore → connected → signal.
	checker := &fakeSyncHealthChecker{
		connected: []bool{false, true, false, true},
	}
	var buf bytes.Buffer

	go func() {
		time.Sleep(100 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)

	if checker.restoreCalls != 2 {
		t.Fatalf("expected 2 restore calls (reset after reconnect), got %d", checker.restoreCalls)
	}
}

func TestSyncHealthLoopRestoreFailureStillRetries(t *testing.T) {
	sigCh := make(chan os.Signal, 1)
	// 3 disconnects: first two restores fail, third succeeds, then connected.
	checker := &fakeSyncHealthChecker{
		connected:  []bool{false, false, false, true},
		restoreErr: []error{errors.New("fail"), errors.New("fail"), nil},
	}
	var buf bytes.Buffer

	go func() {
		time.Sleep(100 * time.Millisecond)
		sigCh <- os.Interrupt
	}()

	runSyncHealthLoopWithConfig(sigCh, &buf, checker, testSyncHealthConfig)

	if checker.restoreCalls != 3 {
		t.Fatalf("expected 3 restore calls, got %d", checker.restoreCalls)
	}
	// The "reconnected" message should only appear once (after successful restore).
	if strings.Count(buf.String(), "reconnected to peer") != 1 {
		t.Fatalf("expected exactly 1 reconnection message, got %q", buf.String())
	}
}
