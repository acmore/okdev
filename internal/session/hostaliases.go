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

const hostAliasesFileName = "host-aliases.json"

// HostAliases records the short-name block okdev last wrote into the session
// pods' /etc/hosts. It exists because the block lives inside the pods, where
// okdev cannot see it: without a record of what was written there is nothing
// to compare a live pod listing against, and a pod the controller recreated
// behind okdev's back looks identical to one that was never touched (#220).
type HostAliases struct {
	At      time.Time         `json:"at"`
	Entries []HostAliasEntry  `json:"entries,omitempty"`
	Extra   map[string]string `json:"extra,omitempty"`
}

// HostAliasEntry pairs an alias with the pod identity it was written for. UID
// is the part that matters: a controller-recreated pod keeps its name and
// gets a new UID and IP, so a name-keyed comparison would miss the recreation
// that this record exists to catch.
type HostAliasEntry struct {
	Alias string `json:"alias"`
	Pod   string `json:"pod"`
	UID   string `json:"uid,omitempty"`
	IP    string `json:"ip,omitempty"`
}

func hostAliasesPath(name string) (string, error) {
	dir, err := SessionDir(name)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, hostAliasesFileName), nil
}

func SaveHostAliases(name string, record HostAliases) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := hostAliasesPath(name)
	if err != nil {
		return err
	}
	payload, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("marshal host alias record: %w", err)
	}
	if err := atomicWriteFile(path, append(payload, '\n'), 0o644); err != nil {
		return fmt.Errorf("write host alias record: %w", err)
	}
	return nil
}

// LoadHostAliases returns the cached record; a zero At reports none.
func LoadHostAliases(name string) (HostAliases, error) {
	if strings.TrimSpace(name) == "" {
		return HostAliases{}, nil
	}
	path, err := hostAliasesPath(name)
	if err != nil {
		return HostAliases{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return HostAliases{}, nil
		}
		return HostAliases{}, fmt.Errorf("read host alias record: %w", err)
	}
	var record HostAliases
	if err := json.Unmarshal(b, &record); err != nil {
		return HostAliases{}, fmt.Errorf("decode host alias record: %w", err)
	}
	return record, nil
}

func ClearHostAliases(name string) error {
	if strings.TrimSpace(name) == "" {
		return nil
	}
	path, err := hostAliasesPath(name)
	if err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("clear host alias record: %w", err)
	}
	return nil
}
