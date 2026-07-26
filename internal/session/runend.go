package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const runEndFileName = "run-end.json"

// Run-end classes. The class is the actionable part: an idle reclaim and a
// preemption call for opposite responses (keep the accelerators busy vs. make
// the workload resilient and chunk long runs), so a report that cannot tell
// them apart sends the reader after the wrong mitigation (#213).
const (
	RunEndClassIdleReclaim = "idle-reclaim"
	RunEndClassEvicted     = "evicted-or-preempted"
	RunEndClassOOM         = "oom-killed"
	RunEndClassFailed      = "container-failed"
	RunEndClassDeleted     = "deleted"
	RunEndClassUnknown     = "unknown"
)

// RunEnd records that a previous run of a session ended and what okdev could
// still tell about why. It is written when a new run replaces one that was not
// torn down on purpose, and read back by the commands the user is already on
// (`up`, `status`) — the cause of death is worthless if it is only reachable
// while the session is still gone.
type RunEnd struct {
	// EndedRunID is the run that disappeared; EndedWorkload its object name.
	EndedRunID    string `json:"endedRunID"`
	EndedWorkload string `json:"endedWorkload,omitempty"`
	// LastSeenAt is when the ended run was last observed alive, DetectedAt
	// when okdev noticed it was replaced.
	LastSeenAt time.Time `json:"lastSeenAt,omitempty"`
	DetectedAt time.Time `json:"detectedAt,omitempty"`
	// SucceededByRunID scopes the report to the run that replaced it: once a
	// third run starts, this record is superseded rather than accumulating.
	SucceededByRunID string `json:"succeededByRunID,omitempty"`
	Class            string `json:"class,omitempty"`
	// Reason is the one-line human explanation; Evidence names where it came
	// from (a cached pod state, or a surviving cluster event).
	Reason   string `json:"reason,omitempty"`
	Evidence string `json:"evidence,omitempty"`
}

func runEndPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, runEndFileName), nil
}

func SaveRunEnd(name string, record RunEnd) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := runEndPath(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal run-end record: %w", err)
	}
	if err := atomicWriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write run-end record: %w", err)
	}
	return nil
}

// LoadRunEnd returns the stored record; an empty EndedRunID reports none.
func LoadRunEnd(name string) (RunEnd, error) {
	if strings.TrimSpace(name) == "" {
		return RunEnd{}, nil
	}
	path, err := runEndPath(name)
	if err != nil {
		return RunEnd{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return RunEnd{}, nil
		}
		return RunEnd{}, fmt.Errorf("read run-end record: %w", err)
	}
	var record RunEnd
	if err := json.Unmarshal(b, &record); err != nil {
		return RunEnd{}, fmt.Errorf("decode run-end record: %w", err)
	}
	return record, nil
}

func ClearRunEnd(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := runEndPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear run-end record: %w", err)
	}
	return nil
}
