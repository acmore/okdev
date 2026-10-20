package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const configTrustFileName = "trusted-configs.json"

// ConfigTrust records the local capabilities (sync roots outside the
// project, credential uploads, reverse forwards, agent forwarding) the user
// approved for a config. A command proceeds when every capability it uses is
// in the approved set, so unrelated edits keep the approval and any new local
// reach asks again where it is used.
type ConfigTrust struct {
	Capabilities []string  `json:"capabilities"`
	ApprovedAt   time.Time `json:"approvedAt"`
}

func configTrustPath() (string, error) {
	root, err := stateRootDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, configTrustFileName), nil
}

// LoadConfigTrust returns approvals keyed by absolute config path.
func LoadConfigTrust() (map[string]ConfigTrust, error) {
	path, err := configTrustPath()
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return map[string]ConfigTrust{}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read config trust: %w", err)
	}
	trust := map[string]ConfigTrust{}
	if err := json.Unmarshal(data, &trust); err != nil {
		return nil, fmt.Errorf("decode config trust: %w", err)
	}
	return trust, nil
}

// SaveConfigTrust records an approval for configPath.
func SaveConfigTrust(configPath string, record ConfigTrust) error {
	trust, err := LoadConfigTrust()
	if err != nil {
		return err
	}
	trust[configPath] = record
	path, err := configTrustPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	payload, err := json.MarshalIndent(trust, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(payload, '\n'), 0o600)
}
