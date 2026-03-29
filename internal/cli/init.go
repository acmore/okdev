package cli

import (
	"context"
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
	var workloadType string
	var manifestPath string
	var injectPaths []string
	var genericPreset string
	var devImageOverride string
	var sidecarImageOverride string
	var syncLocalOverride string
	var syncRemoteOverride string
	var sshUserOverride string
	var stignorePreset string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter .okdev.yaml config",
		Example: `  # Interactive setup (prompts for each field)
  okdev init

  # Scaffold a Job workload with starter manifest
  okdev init --workload job

  # Generic deployment with preset
  okdev init --workload generic --generic-preset deployment

  # Go project with Go-oriented sync ignores
  okdev init --template basic --stignore-preset go

  # Non-interactive with explicit values
  okdev init --yes --name my-project --namespace dev`,
		RunE: func(cmd *cobra.Command, args []string) error {
			vars := config.NewTemplateVars()
			overrides := InitOverrides{
				Name:          nameOverride,
				Namespace:     nsOverride,
				WorkloadType:  workloadType,
				ManifestPath:  manifestPath,
				InjectPaths:   injectPaths,
				GenericPreset: genericPreset,
				DevImage:      devImageOverride,
				SidecarImage:  sidecarImageOverride,
				SyncLocal:     syncLocalOverride,
				SyncRemote:    syncRemoteOverride,
				SSHUser:       sshUserOverride,
			}
			applyOverrides(vars, overrides)
			applyWorkloadDefaults(vars)

			if err := promptInteractive(vars, overrides, cmd.InOrStdin(), cmd.OutOrStdout(), yes, isTerminalReader(cmd.InOrStdin())); err != nil {
				return err
			}
			applyWorkloadDefaults(vars)

			if err := validateInitWorkloadVars(vars); err != nil {
				return err
			}

			target := opts.ConfigPath
			if target == "" {
				target = defaultInitTargetPath(templateRef, vars)
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve output path %q: %w", target, err)
			}
			if _, err := os.Stat(abs); err == nil && !force {
				return fmt.Errorf("config already exists at %q (use --force to overwrite)", abs)
			}

			rendered, err := config.RenderTemplateContext(context.Background(), templateRef, vars)
			if err != nil {
				return err
			}

			if err := validateRenderedInitConfig(rendered, templateRef, vars); err != nil {
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
				resolvedPreset = detectSTIgnorePreset(config.RootDir(abs))
			}
			stignorePath, wroteSTIgnore, err := writeInitSTIgnore(abs, []byte(rendered), templateRef, resolvedPreset, force)
			if err != nil {
				return err
			}
			scaffolded, err := scaffoldInitWorkload(abs, templateRef, vars, force)
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
			for _, path := range scaffolded {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", path)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().StringVar(&templateRef, "template", "", initTemplateUsage())
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept all defaults")
	cmd.Flags().StringVar(&nameOverride, "name", "", "Environment name")
	cmd.Flags().StringVar(&nsOverride, "namespace", "", "Namespace")
	cmd.Flags().StringVar(&workloadType, "workload", "pod", "Workload type: pod, job, pytorchjob, generic")
	cmd.Flags().StringVar(&manifestPath, "manifest-path", "", "Path to workload manifest")
	cmd.Flags().StringArrayVar(&injectPaths, "inject-path", nil, "Workload inject path (repeatable)")
	cmd.Flags().StringVar(&genericPreset, "generic-preset", "", "Optional generic scaffold preset, for example deployment")
	cmd.Flags().StringVar(&devImageOverride, "dev-image", "", "Dev container image for pod workloads")
	cmd.Flags().StringVar(&sidecarImageOverride, "sidecar-image", "", "Sidecar image")
	cmd.Flags().StringVar(&syncLocalOverride, "sync-local", "", "Local sync path")
	cmd.Flags().StringVar(&syncRemoteOverride, "sync-remote", "", "Remote sync path")
	cmd.Flags().StringVar(&sshUserOverride, "ssh-user", "", "SSH user")
	cmd.Flags().StringVar(&stignorePreset, "stignore-preset", "", "Local .stignore preset: default|python|node|go|rust")
	return cmd
}

func initTemplateUsage() string {
	names := append([]string{}, config.BuiltinTemplateNames()...)
	if userNames, err := config.UserTemplateNames(); err == nil {
		names = append(names, userNames...)
	}
	if len(names) == 0 {
		return "Template: file path or URL"
	}
	return fmt.Sprintf("Template: %s, file path, or URL", strings.Join(names, "|"))
}

func defaultInitTargetPath(templateRef string, vars *config.TemplateVars) string {
	if scaffoldsInitWorkload(templateRef, vars) {
		return config.FolderFile
	}
	return config.DefaultFile
}

func applyWorkloadDefaults(vars *config.TemplateVars) {
	switch strings.TrimSpace(vars.WorkloadType) {
	case "", "pod":
		vars.WorkloadType = "pod"
		vars.ManifestPath = ""
		vars.InjectPaths = nil
		vars.AttachContainer = ""
	case "job":
		if strings.TrimSpace(vars.ManifestPath) == "" {
			vars.ManifestPath = ".okdev/job.yaml"
		}
		if len(vars.InjectPaths) == 0 {
			vars.InjectPaths = []string{"spec.template"}
		}
		if strings.TrimSpace(vars.AttachContainer) == "" {
			vars.AttachContainer = "dev"
		}
	case "pytorchjob":
		if strings.TrimSpace(vars.ManifestPath) == "" {
			vars.ManifestPath = ".okdev/pytorchjob.yaml"
		}
		if len(vars.InjectPaths) == 0 {
			vars.InjectPaths = []string{"spec.pytorchReplicaSpecs.Master.template"}
		}
		if strings.TrimSpace(vars.AttachContainer) == "" {
			vars.AttachContainer = "dev"
		}
	case "generic":
		if strings.TrimSpace(vars.GenericPreset) == "deployment" {
			if strings.TrimSpace(vars.ManifestPath) == "" {
				vars.ManifestPath = ".okdev/deployment.yaml"
			}
			if len(vars.InjectPaths) == 0 {
				vars.InjectPaths = []string{"spec.template"}
			}
			if strings.TrimSpace(vars.AttachContainer) == "" {
				vars.AttachContainer = "dev"
			}
		}
	}
}

var knownGenericPresets = map[string]bool{
	"deployment": true,
}

func validateInitWorkloadVars(vars *config.TemplateVars) error {
	switch strings.TrimSpace(vars.WorkloadType) {
	case "pod", "job", "pytorchjob", "generic":
	default:
		return fmt.Errorf("unsupported workload type %q", vars.WorkloadType)
	}
	if vars.WorkloadType == "generic" {
		if strings.TrimSpace(vars.ManifestPath) == "" {
			return fmt.Errorf("--manifest-path is required when --workload=generic")
		}
		if len(vars.InjectPaths) == 0 {
			return fmt.Errorf("at least one --inject-path is required when --workload=generic")
		}
	}
	if preset := strings.TrimSpace(vars.GenericPreset); preset != "" && !knownGenericPresets[preset] {
		return fmt.Errorf("unknown --generic-preset %q; known presets: deployment", preset)
	}
	return nil
}

func validateRenderedInitConfig(rendered, templateRef string, vars *config.TemplateVars) error {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		return fmt.Errorf("parse generated config: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}
	if vars.WorkloadType != "pod" && !usesBuiltinBasicTemplate(templateRef) && strings.TrimSpace(cfg.Spec.Workload.ManifestPath) == "" {
		return fmt.Errorf("custom template must render spec.workload for workload type %q", vars.WorkloadType)
	}
	return nil
}

func scaffoldInitWorkload(configPath, templateRef string, vars *config.TemplateVars, force bool) ([]string, error) {
	if !scaffoldsInitWorkload(templateRef, vars) {
		return nil, nil
	}
	var templatePath string
	switch vars.WorkloadType {
	case "job":
		templatePath = "templates/manifests/job.yaml.tmpl"
	case "pytorchjob":
		templatePath = "templates/manifests/pytorchjob.yaml.tmpl"
	case "generic":
		if strings.TrimSpace(vars.GenericPreset) == "deployment" {
			templatePath = "templates/manifests/deployment.yaml.tmpl"
		}
	}
	if templatePath == "" || strings.TrimSpace(vars.ManifestPath) == "" {
		return nil, nil
	}
	target := vars.ManifestPath
	if !filepath.IsAbs(target) {
		target = filepath.Join(config.RootDir(configPath), target)
	}
	target = filepath.Clean(target)
	if _, err := os.Stat(target); err == nil && !force {
		return nil, nil
	}
	rendered, err := config.RenderEmbeddedTemplate(templatePath, vars)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return nil, fmt.Errorf("create manifest directory: %w", err)
	}
	if err := os.WriteFile(target, []byte(rendered), 0o644); err != nil {
		return nil, fmt.Errorf("write scaffolded manifest %q: %w", target, err)
	}
	return []string{target}, nil
}

func usesBuiltinBasicTemplate(ref string) bool {
	return strings.TrimSpace(ref) == "" || strings.TrimSpace(ref) == "basic"
}

func scaffoldsInitWorkload(templateRef string, vars *config.TemplateVars) bool {
	if !usesBuiltinBasicTemplate(templateRef) {
		return false
	}
	switch strings.TrimSpace(vars.WorkloadType) {
	case "job", "pytorchjob":
		return true
	case "generic":
		return strings.TrimSpace(vars.GenericPreset) == "deployment"
	default:
		return false
	}
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
		localRoot = filepath.Join(config.RootDir(configPath), localRoot)
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
