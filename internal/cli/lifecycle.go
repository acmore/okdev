package cli

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
)

func resolvePostCreateCommand(cfg *config.DevEnvironment, configPath string) string {
	if cfg != nil && strings.TrimSpace(cfg.Spec.Lifecycle.PostCreate) != "" {
		return strings.TrimSpace(cfg.Spec.Lifecycle.PostCreate)
	}
	root := configRootFromPath(configPath)
	if root == "" || cfg == nil {
		return ""
	}
	script := filepath.Join(root, ".okdev", "post-create.sh")
	if st, err := os.Stat(script); err == nil && !st.IsDir() {
		return filepath.Join(cfg.EffectiveWorkspaceMountPath(configPath), ".okdev", "post-create.sh")
	}
	return ""
}

func resolvePostSyncCommand(cfg *config.DevEnvironment, configPath string) string {
	if cfg != nil && strings.TrimSpace(cfg.Spec.Lifecycle.PostSync) != "" {
		return strings.TrimSpace(cfg.Spec.Lifecycle.PostSync)
	}
	root := configRootFromPath(configPath)
	if root == "" || cfg == nil {
		return ""
	}
	script := filepath.Join(root, ".okdev", "post-sync.sh")
	if st, err := os.Stat(script); err == nil && !st.IsDir() {
		return filepath.Join(cfg.EffectiveWorkspaceMountPath(configPath), ".okdev", "post-sync.sh")
	}
	return ""
}

func resolvePreStopCommand(cfg *config.DevEnvironment, configPath string) string {
	if cfg != nil && strings.TrimSpace(cfg.Spec.Lifecycle.PreStop) != "" {
		return strings.TrimSpace(cfg.Spec.Lifecycle.PreStop)
	}
	root := configRootFromPath(configPath)
	if root == "" || cfg == nil {
		return ""
	}
	script := filepath.Join(root, ".okdev", "pre-stop.sh")
	if st, err := os.Stat(script); err == nil && !st.IsDir() {
		return filepath.Join(cfg.EffectiveWorkspaceMountPath(configPath), ".okdev", "pre-stop.sh")
	}
	return ""
}

func configRootFromPath(configPath string) string {
	return config.RootDir(configPath)
}
