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

const lastSeenFileName = "last-seen.json"

// LastSeen is the most recent live snapshot of a session's pods, written on
// status refreshes. When the session later vanishes (TTL, quota, admin
// delete), this is the only local record of what the pods looked like —
// `okdev status` surfaces it instead of a bare "not found". Intentional
// lifecycle transitions (up, down) clear it so it only ever describes an
// external death.
type LastSeen struct {
	At        time.Time         `json:"at"`
	Namespace string            `json:"namespace,omitempty"`
	Workload  LastSeenWorkload  `json:"workload,omitempty"`
	Pods      []LastSeenPod     `json:"pods,omitempty"`
	Extra     map[string]string `json:"extra,omitempty"`
}

type LastSeenWorkload struct {
	APIVersion string `json:"apiVersion,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Name       string `json:"name,omitempty"`
}

type LastSeenPod struct {
	Name     string             `json:"name"`
	Phase    string             `json:"phase,omitempty"`
	Ready    string             `json:"ready,omitempty"`
	Reason   string             `json:"reason,omitempty"`
	Deleting bool               `json:"deleting,omitempty"`
	Node     string             `json:"node,omitempty"`
	Issues   []LastSeenPodIssue `json:"issues,omitempty"`
}

type LastSeenPodIssue struct {
	Container string `json:"container"`
	Reason    string `json:"reason,omitempty"`
	ExitCode  int32  `json:"exitCode,omitempty"`
}

func lastSeenPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, lastSeenFileName), nil
}

func SaveLastSeen(name string, snapshot LastSeen) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := lastSeenPath(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return fmt.Errorf("marshal last-seen snapshot: %w", err)
	}
	if err := atomicWriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write last-seen snapshot: %w", err)
	}
	return nil
}

// LoadLastSeen returns the cached snapshot; a zero At reports no snapshot.
func LoadLastSeen(name string) (LastSeen, error) {
	if strings.TrimSpace(name) == "" {
		return LastSeen{}, nil
	}
	path, err := lastSeenPath(name)
	if err != nil {
		return LastSeen{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return LastSeen{}, nil
		}
		return LastSeen{}, fmt.Errorf("read last-seen snapshot: %w", err)
	}
	var snapshot LastSeen
	if err := json.Unmarshal(b, &snapshot); err != nil {
		return LastSeen{}, fmt.Errorf("decode last-seen snapshot: %w", err)
	}
	return snapshot, nil
}

func ClearLastSeen(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := lastSeenPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear last-seen snapshot: %w", err)
	}
	return nil
}
