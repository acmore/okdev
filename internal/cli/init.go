package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"
)

func newInitCmd(opts *Options) *cobra.Command {
	var force bool
	var templateRef string
	var yes bool
	var nameOverride string
	var nsOverride string
	var sidecarImageOverride string
	var syncLocalOverride string
	var syncRemoteOverride string
	var sshUserOverride string
	var stignorePreset string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter .okdev.yaml config",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := config.DefaultFile
			if opts.ConfigPath != "" {
				target = opts.ConfigPath
			}

			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve output path %q: %w", target, err)
			}

			if _, err := os.Stat(abs); err == nil && !force {
				return fmt.Errorf("config already exists at %q (use --force to overwrite)", abs)
			}

			vars := config.NewTemplateVars()
			overrides := InitOverrides{
				Name:         nameOverride,
				Namespace:    nsOverride,
				SidecarImage: sidecarImageOverride,
				SyncLocal:    syncLocalOverride,
				SyncRemote:   syncRemoteOverride,
				SSHUser:      sshUserOverride,
			}
			applyOverrides(vars, overrides)

			if err := promptInteractive(vars, overrides, cmd.InOrStdin(), cmd.OutOrStdout(), yes, isTerminalReader(cmd.InOrStdin())); err != nil {
				return err
			}

			rendered, err := config.RenderTemplate(templateRef, vars)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
			if err := os.WriteFile(abs, []byte(rendered), 0o644); err != nil {
				return fmt.Errorf("write config %q: %w", abs, err)
			}
			resolvedPreset := strings.TrimSpace(stignorePreset)
			if resolvedPreset == "" {
				resolvedPreset = detectSTIgnorePreset(filepath.Dir(abs))
			}
			stignorePath, wroteSTIgnore, err := writeInitSTIgnore(abs, []byte(rendered), templateRef, resolvedPreset, force)
			if err != nil {
				return err
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", abs)
			if resolvedPreset != "" {
				fmt.Fprintf(cmd.OutOrStdout(), "Using .stignore preset: %s\n", resolvedPreset)
			}
			if wroteSTIgnore {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", stignorePath)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().StringVar(&templateRef, "template", "basic", "Template: built-in name, file path, or URL")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept all defaults")
	cmd.Flags().StringVar(&nameOverride, "name", "", "Environment name")
	cmd.Flags().StringVar(&nsOverride, "namespace", "", "Namespace")
	cmd.Flags().StringVar(&sidecarImageOverride, "sidecar-image", "", "Sidecar image")
	cmd.Flags().StringVar(&syncLocalOverride, "sync-local", "", "Local sync path")
	cmd.Flags().StringVar(&syncRemoteOverride, "sync-remote", "", "Remote sync path")
	cmd.Flags().StringVar(&sshUserOverride, "ssh-user", "", "SSH user")
	cmd.Flags().StringVar(&stignorePreset, "stignore-preset", "", "Local .stignore preset: default|python|node|go|rust")
	return cmd
}

func detectSTIgnorePreset(dir string) string {
	type candidate struct {
		preset  string
		markers []string
	}

	candidates := []candidate{
		{preset: "go", markers: []string{"go.mod"}},
		{preset: "node", markers: []string{"package.json"}},
		{preset: "rust", markers: []string{"Cargo.toml"}},
		{preset: "python", markers: []string{"pyproject.toml", "requirements.txt", "uv.lock", "poetry.lock"}},
	}

	for _, candidate := range candidates {
		for _, marker := range candidate.markers {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return candidate.preset
			}
		}
	}

	return ""
}

func writeInitSTIgnore(configPath string, rendered []byte, templateRef string, stignorePreset string, force bool) (string, bool, error) {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal(rendered, &cfg); err != nil {
		return "", false, fmt.Errorf("parse generated config for .stignore: %w", err)
	}
	cfg.SetDefaults()
	patterns := config.BuiltinTemplateLocalIgnores(templateRef)
	if preset := strings.TrimSpace(stignorePreset); preset != "" {
		patterns = config.STIgnorePreset(preset)
		if patterns == nil {
			return "", false, fmt.Errorf("unknown .stignore preset %q", preset)
		}
	}
	content, ok := buildSTIgnoreContent(patterns)
	if !ok {
		return "", false, nil
	}
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.WorkspaceMountPath())
	if err != nil {
		return "", false, fmt.Errorf("resolve generated sync paths for .stignore: %w", err)
	}
	if len(pairs) == 0 {
		return "", false, nil
	}
	localRoot := pairs[0].Local
	if !filepath.IsAbs(localRoot) {
		localRoot = filepath.Join(filepath.Dir(configPath), localRoot)
	}
	localRoot = filepath.Clean(localRoot)
	if err := os.MkdirAll(localRoot, 0o755); err != nil {
		return "", false, fmt.Errorf("create local sync root for .stignore: %w", err)
	}
	stignorePath := filepath.Join(localRoot, ".stignore")
	if _, err := os.Stat(stignorePath); err == nil && !force {
		return stignorePath, false, nil
	}
	if err := os.WriteFile(stignorePath, []byte(content), 0o644); err != nil {
		return "", false, fmt.Errorf("write .stignore %q: %w", stignorePath, err)
	}
	return stignorePath, true, nil
}
