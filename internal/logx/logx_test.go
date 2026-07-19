package logx

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"k8s.io/klog/v2"
)

func TestConfigureVerbosity(t *testing.T) {
	Configure(false)
	if slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be disabled when verbose=false")
	}
	if !slog.Default().Enabled(context.Background(), slog.LevelInfo) {
		t.Fatal("info should be enabled when verbose=false")
	}

	Configure(true)
	if !slog.Default().Enabled(context.Background(), slog.LevelDebug) {
		t.Fatal("debug should be enabled when verbose=true")
	}
}

// Regression for issue #176: klog duplicates ERROR-and-above records onto
// os.Stderr through its default stderrthreshold=ERROR even when LogToStderr
// is false and severity outputs are routed elsewhere. That is how client-go
// transport errors ("Websocket Ping failed") leaked into exec's terminal
// streams and corrupted parsed command output.
func TestConfigureRoutesKlogErrorsAwayFromStderr(t *testing.T) {
	var captured bytes.Buffer
	logWriterMu.Lock()
	prev := logWriter
	logWriter = &captured
	logWriterMu.Unlock()
	defer func() {
		logWriterMu.Lock()
		logWriter = prev
		logWriterMu.Unlock()
	}()

	origStderr := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stderr = w
	defer func() { os.Stderr = origStderr }()

	Configure(false)
	klog.ErrorS(errors.New("ping timeout"), "Websocket Ping failed")
	klog.Flush()

	_ = w.Close()
	os.Stderr = origStderr
	leaked, err := io.ReadAll(r)
	if err != nil {
		t.Fatal(err)
	}

	if strings.Contains(string(leaked), "Websocket Ping failed") {
		t.Fatalf("klog ERROR leaked to stderr: %q", leaked)
	}
	if !strings.Contains(captured.String(), "Websocket Ping failed") {
		t.Fatalf("klog ERROR not routed to okdev log writer, got %q", captured.String())
	}
}

func TestOpenRotatingLogRotatesOversizedFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rotateLogIfNeeded(path, 32, 5); err != nil {
		t.Fatalf("rotateLogIfNeeded error: %v", err)
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatalf("open rotated log error: %v", err)
	}
	_ = f.Close()

	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated backup file: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("expected new active log file: %v", err)
	}
	if info.Size() != 0 {
		t.Fatalf("expected empty active log after rotation, got %d bytes", info.Size())
	}
}

func TestRotateLogIfNeededCapsBackups(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "okdev.log")
	if err := os.WriteFile(path, []byte(strings.Repeat("x", 64)), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".1", []byte("one"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+".2", []byte("two"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := rotateLogIfNeeded(path, 32, 2); err != nil {
		t.Fatalf("rotateLogIfNeeded error: %v", err)
	}

	if _, err := os.Stat(path + ".3"); !os.IsNotExist(err) {
		t.Fatalf("expected no third backup, got err=%v", err)
	}
	if _, err := os.Stat(path + ".2"); err != nil {
		t.Fatalf("expected second backup to remain after rotation: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected first backup to exist after rotation: %v", err)
	}
}
