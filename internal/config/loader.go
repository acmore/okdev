package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"sigs.k8s.io/yaml"
)

const (
	DefaultFile = ".okdev.yaml"
	FolderFile  = ".okdev/okdev.yaml"
	LegacyFile  = "okdev.yaml"
)

func Load(configPath string) (*DevEnvironment, string, error) {
	path, err := ResolvePath(configPath)
	if err != nil {
		return nil, "", err
	}

	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, "", fmt.Errorf("read config %q: %w", path, err)
	}
	if removed := removedSyncIgnoreField(raw); removed != "" {
		msg := fmt.Sprintf("%s is removed; manage local ignores with .stignore in the synced local workspace instead", removed)
		if removed == "spec.sync.remoteExclude" {
			msg = "spec.sync.remoteExclude is removed; use spec.sync.remoteIgnore for managed remote .stignore patterns"
		}
		return nil, "", fmt.Errorf("validate config %q: %w", path, &MigrationEligibleError{Err: errors.New(msg)})
	}

	var cfg DevEnvironment
	if err := yaml.Unmarshal(raw, &cfg); err != nil {
		return nil, "", fmt.Errorf("parse config %q: %w", path, err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, "", fmt.Errorf("validate config %q: %w", path, err)
	}

	return &cfg, path, nil
}

func removedSyncIgnoreField(raw []byte) string {
	var payload map[string]any
	if err := yaml.Unmarshal(raw, &payload); err != nil {
		return ""
	}
	specMap, ok := payload["spec"].(map[string]any)
	if !ok {
		return ""
	}
	syncMap, ok := specMap["sync"].(map[string]any)
	if !ok {
		return ""
	}
	if _, ok := syncMap["exclude"]; ok {
		return "spec.sync.exclude"
	}
	if _, ok := syncMap["remoteExclude"]; ok {
		return "spec.sync.remoteExclude"
	}
	return ""
}

func ResolvePath(configPath string) (string, error) {
	if configPath != "" {
		abs, err := filepath.Abs(configPath)
		if err != nil {
			return "", fmt.Errorf("resolve config path %q: %w", configPath, err)
		}
		if _, err := os.Stat(abs); err != nil {
			return "", fmt.Errorf("config not found at %q", abs)
		}
		return abs, nil
	}

	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}

	gitRoot, _ := findGitRoot(wd)
	p, err := discoverInParents(wd, gitRoot, FolderFile, DefaultFile, LegacyFile)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", fmt.Errorf("no config found; create %s or pass -c/--config", DefaultFile)
		}
		return "", err
	}
	return p, nil
}

func RootDir(configPath string) string {
	p := strings.TrimSpace(configPath)
	if p == "" {
		return ""
	}
	dir := filepath.Dir(p)
	if filepath.Base(dir) == ".okdev" && filepath.Base(p) == "okdev.yaml" {
		return filepath.Dir(dir)
	}
	return dir
}

func ManifestDir(configPath string) string {
	p := strings.TrimSpace(configPath)
	if p == "" {
		return ""
	}
	return filepath.Dir(p)
}

func ResolveWorkloadManifestPath(configPath, manifestPath string) string {
	p := strings.TrimSpace(manifestPath)
	if p == "" {
		return ""
	}
	if filepath.IsAbs(p) {
		return p
	}

	manifestDir := ManifestDir(configPath)
	if strings.TrimSpace(manifestDir) == "" {
		manifestDir = filepath.Dir(configPath)
	}
	manifestCandidate := filepath.Clean(filepath.Join(manifestDir, p))
	if _, err := os.Stat(manifestCandidate); err == nil {
		return manifestCandidate
	}

	rootDir := RootDir(configPath)
	if strings.TrimSpace(rootDir) != "" {
		rootCandidate := filepath.Clean(filepath.Join(rootDir, p))
		if _, err := os.Stat(rootCandidate); err == nil {
			return rootCandidate
		}
	}

	return manifestCandidate
}

func discoverInParents(start, stopAt string, names ...string) (string, error) {
	current := start
	for {
		for _, name := range names {
			candidate := filepath.Join(current, name)
			if st, err := os.Stat(candidate); err == nil && !st.IsDir() {
				return candidate, nil
			}
		}
		if stopAt != "" && current == stopAt {
			break
		}

		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", os.ErrNotExist
}

func findGitRoot(start string) (string, error) {
	current := start
	for {
		dotgit := filepath.Join(current, ".git")
		if _, err := os.Stat(dotgit); err == nil {
			return current, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	return "", os.ErrNotExist
}
