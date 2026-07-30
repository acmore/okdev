package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const workloadPinName = "workload"

// WorkloadPinPath is where a session records which workload profile it runs.
//
// The pin is per session rather than per repo: two sessions in one repo would
// otherwise contend for a single slot, and a repo-level pin could be moved by a
// git pull, silently swapping out a running workload.
func WorkloadPinPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, workloadPinName), nil
}

func SaveWorkloadProfile(name, profile string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	if strings.TrimSpace(profile) == "" {
		return ClearWorkloadProfile(name)
	}
	p, err := WorkloadPinPath(name)
	if err != nil {
		return err
	}
	if err := os.WriteFile(p, []byte(strings.TrimSpace(profile)+"\n"), 0o644); err != nil {
		return fmt.Errorf("write workload pin: %w", err)
	}
	return nil
}

func LoadWorkloadProfile(name string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", nil
	}
	p, err := WorkloadPinPath(name)
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read workload pin: %w", err)
	}
	return strings.TrimSpace(string(b)), nil
}

func ClearWorkloadProfile(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	p, err := WorkloadPinPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear workload pin: %w", err)
	}
	return nil
}
