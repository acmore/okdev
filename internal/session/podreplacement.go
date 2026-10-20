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

const podReplacementFileName = "pod-replacement.json"

// PodReplacement records that pods of the live run ended without okdev being
// asked — evicted, preempted, drained, reclaimed — and their controller
// replaced them under the same run-id (#325). The run-level RunEnd cannot see
// this: the run never ended. It is written before the last-seen snapshot is
// refreshed, because that snapshot is the only local record of the old pods,
// and it is dropped by the next intentional up/restart/down.
type PodReplacement struct {
	RunID string        `json:"runID"`
	Ended []ReplacedPod `json:"ended"`
	// ReplacedBy names the live pods that were not in the snapshot.
	ReplacedBy []string  `json:"replacedBy,omitempty"`
	LastSeenAt time.Time `json:"lastSeenAt,omitempty"`
	DetectedAt time.Time `json:"detectedAt,omitempty"`
	Class      string    `json:"class,omitempty"`
	Reason     string    `json:"reason,omitempty"`
	Evidence   string    `json:"evidence,omitempty"`
}

// ReplacedPod identifies one ended pod. UID tells a same-name recreation
// (PyTorchJob, StatefulSet) apart from the pod it replaced.
type ReplacedPod struct {
	Name  string `json:"name"`
	UID   string `json:"uid,omitempty"`
	Node  string `json:"node,omitempty"`
	Phase string `json:"phase,omitempty"`
}

func podReplacementPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, podReplacementFileName), nil
}

func SavePodReplacement(name string, record PodReplacement) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := podReplacementPath(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal pod-replacement record: %w", err)
	}
	if err := atomicWriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write pod-replacement record: %w", err)
	}
	return nil
}

// LoadPodReplacement returns the stored record; no Ended pods reports none.
func LoadPodReplacement(name string) (PodReplacement, error) {
	if strings.TrimSpace(name) == "" {
		return PodReplacement{}, nil
	}
	path, err := podReplacementPath(name)
	if err != nil {
		return PodReplacement{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return PodReplacement{}, nil
		}
		return PodReplacement{}, fmt.Errorf("read pod-replacement record: %w", err)
	}
	var record PodReplacement
	if err := json.Unmarshal(b, &record); err != nil {
		return PodReplacement{}, fmt.Errorf("decode pod-replacement record: %w", err)
	}
	return record, nil
}

func ClearPodReplacement(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := podReplacementPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear pod-replacement record: %w", err)
	}
	return nil
}
