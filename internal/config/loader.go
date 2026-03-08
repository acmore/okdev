package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
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
