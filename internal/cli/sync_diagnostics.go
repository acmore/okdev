package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

type syncExitRecord struct {
	PID     int       `json:"pid"`
	At      time.Time `json:"at"`
	Reason  string    `json:"reason"`
	LogPath string    `json:"logPath"`
}

func syncDiagnosticPath(sessionName, name string) string {
	home, err := localSyncthingStatusHome(sessionName)
	if err != nil {
		return ""
	}
	return filepath.Join(home, name)
}

func writeSyncDiagnostic(path string, data []byte) error {
	if path == "" {
		return errors.New("missing diagnostic path")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".sync-diagnostic-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(f.Name(), path)
}

func recordSyncExit(sessionName, reason string, err error) {
	if reason == "" {
		switch {
		case err == nil:
			reason = "sync command exited without error"
		case errors.Is(err, context.Canceled):
			reason = "sync command cancelled"
		case errors.Is(err, context.DeadlineExceeded):
			reason = "sync command timed out"
		case apierrors.IsForbidden(err):
			reason = "Kubernetes access forbidden"
		case apierrors.IsUnauthorized(err):
			reason = "Kubernetes authentication rejected"
		default:
			reason = "sync command failed; see background log"
		}
	}
	logPath, _ := syncthingBackgroundLogPath(sessionName)
	data, _ := json.Marshal(syncExitRecord{PID: os.Getpid(), At: time.Now().UTC(), Reason: reason, LogPath: logPath})
	_ = writeSyncDiagnostic(syncDiagnosticPath(sessionName, "sync.last-exit.json"), data)
}

func readSyncExit(sessionName string) *syncExitRecord {
	data, err := os.ReadFile(syncDiagnosticPath(sessionName, "sync.last-exit.json"))
	if err != nil {
		return nil
	}
	var record syncExitRecord
	if json.Unmarshal(data, &record) != nil || record.PID <= 0 || record.At.IsZero() {
		return nil
	}
	return &record
}

func stoppedSyncReason(sessionName string, pid int) string {
	reason := "exit cause unavailable"
	if record := readSyncExit(sessionName); record != nil && record.PID == pid {
		reason = record.Reason
	}
	logPath, _ := syncthingBackgroundLogPath(sessionName)
	return fmt.Sprintf("sync is not running (pid %d; %s); log: %s", pid, reason, logPath)
}

func waitForReadySync(ctx context.Context, sessionName string, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	for {
		status, reason := checkSyncHealthContext(ctx, sessionName)
		if status == syncHealthActive && ctx.Err() == nil {
			return nil
		}
		if status == syncHealthStopped || status == syncHealthPaused || ctx.Err() != nil {
			return fmt.Errorf("sync readiness degraded: %s; workload retained; repair with `okdev sync` then retry `okdev up`", reason)
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return fmt.Errorf("sync readiness degraded: %s: %w; workload retained; repair with `okdev sync` then retry `okdev up`", reason, ctx.Err())
		case <-timer.C:
		}
	}
}

func syncPreflightWarning(sessionName string, status syncHealthStatus, reason, subject string) string {
	path := syncDiagnosticPath(sessionName, "sync.warning")
	if status == syncHealthActive {
		_ = os.Remove(path)
		return ""
	}
	key := string(status) + "\n" + reason
	if data, err := os.ReadFile(path); err == nil {
		stamp, previous, ok := strings.Cut(string(data), "\n")
		at, _ := strconv.ParseInt(stamp, 10, 64)
		elapsed := time.Since(time.Unix(at, 0))
		if ok && previous == key && elapsed >= 0 && elapsed < time.Minute {
			return fmt.Sprintf("warning: sync still unhealthy (%s); command may run stale code; see `okdev status --details`", status)
		}
	}
	_ = writeSyncDiagnostic(path, []byte(strconv.FormatInt(time.Now().Unix(), 10)+"\n"+key))
	return syncStalenessWarning(status, reason, subject) + "; diagnostics: `okdev status --details`"
}
