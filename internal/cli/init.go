package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

func newInitCmd(opts *Options) *cobra.Command {
	var force bool
	var templateRef string
	var yes bool
	var nameOverride string
	var nsOverride string
	var contextOverride string
	var workloadType string
	var manifestPath string
	var injectPaths []string
	var genericPreset string
	var devImageOverride string
	var sidecarImageOverride string
	var syncLocalOverride string
	var syncRemoteOverride string
	var sshUserOverride string
	var shellOverride string
	var stignorePreset string
	var setFlags []string

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
  okdev init --yes --name my-project --namespace dev

  # Use a specific kube context
  okdev init --context my-cluster --namespace staging`,
		RunE: func(cmd *cobra.Command, args []string) error {
			vars := config.NewTemplateVars()
			overrides := InitOverrides{
				Name:          nameOverride,
				Namespace:     nsOverride,
				KubeContext:   contextOverride,
				WorkloadType:  workloadType,
				ManifestPath:  manifestPath,
				InjectPaths:   injectPaths,
				GenericPreset: genericPreset,
				DevImage:      devImageOverride,
				SidecarImage:  sidecarImageOverride,
				SyncLocal:     syncLocalOverride,
				SyncRemote:    syncRemoteOverride,
				SSHUser:       sshUserOverride,
				Shell:         shellOverride,
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

			projectDir, err := initProjectDir(opts.ConfigPath)
			if err != nil {
				return err
			}
			rawTemplate, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
			if err != nil {
				return err
			}
			meta, body, err := config.ParseFrontmatter(rawTemplate)
			if err != nil {
				return err
			}
			sets := parseSetFlags(setFlags)
			warnUnknownTemplateSets(cmd.ErrOrStderr(), meta, sets)
			customVars, err := resolveInitTemplateVars(meta, sets, nil, yes, isTerminalReader(cmd.InOrStdin()), cmd.InOrStdin(), cmd.OutOrStdout())
			if err != nil {
				return err
			}

			rendered, err := config.RenderTemplateContent("okdev", body, vars, customVars)
			if err != nil {
				return err
			}

			target := opts.ConfigPath
			if target == "" {
				target = defaultInitTargetPath(vars, meta, rendered)
			}
			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve output path %q: %w", target, err)
			}
			projectDir = config.RootDir(abs)
			if _, err := os.Stat(abs); err == nil && !force {
				return fmt.Errorf("config already exists at %q (use --force to overwrite)", abs)
			}
			normalizeInitManifestPathForTarget(abs, vars, overrides.hasManifestPath())

			rendered, err = config.RenderTemplateContent("okdev", body, vars, customVars)
			if err != nil {
				return err
			}
			rendered, err = persistTemplateRefIfNeeded(rendered, templateRef, customVars, projectDir)
			if err != nil {
				return err
			}

			if err := validateRenderedInitConfig(rendered, templateRef, vars, projectDir); err != nil {
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
			stignorePath, wroteSTIgnore, err := writeInitSTIgnore(abs, []byte(rendered), templateRef, resolvedPreset, force, projectDir)
			if err != nil {
				return err
			}
			scaffolded, err := scaffoldInitTemplateFiles(abs, templateRef, meta, vars, customVars, force, projectDir)
			if err != nil {
				return err
			}
			if len(scaffolded) == 0 {
				scaffolded, err = scaffoldInitWorkload(abs, templateRef, vars, force, projectDir)
				if err != nil {
					return err
				}
			}

			zshFiles, err := scaffoldZshFiles(abs, vars, force, cmd.OutOrStdout())
			if err != nil {
				return err
			}
			scaffolded = append(scaffolded, zshFiles...)

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
	cmd.Flags().StringVar(&contextOverride, "context", "", "Kubeconfig context (defaults to active context)")
	cmd.Flags().StringVar(&workloadType, "workload", "pod", "Workload type: pod, job, pytorchjob, generic")
	cmd.Flags().StringVar(&manifestPath, "manifest-path", "", "Path to workload manifest")
	cmd.Flags().StringArrayVar(&injectPaths, "inject-path", nil, "Workload inject path (repeatable)")
	cmd.Flags().StringVar(&genericPreset, "generic-preset", "", "Optional generic scaffold preset, for example deployment")
	cmd.Flags().StringVar(&devImageOverride, "dev-image", "", "Dev container image for pod workloads")
	cmd.Flags().StringVar(&sidecarImageOverride, "sidecar-image", "", "Sidecar image")
	cmd.Flags().StringVar(&syncLocalOverride, "sync-local", "", "Local sync path")
	cmd.Flags().StringVar(&syncRemoteOverride, "sync-remote", "", "Remote sync path")
	cmd.Flags().StringVar(&sshUserOverride, "ssh-user", "", "SSH user")
	cmd.Flags().StringVar(&shellOverride, "shell", "", "Shell for interactive SSH sessions (e.g., /bin/zsh)")
	cmd.Flags().StringVar(&stignorePreset, "stignore-preset", "", "Local .stignore preset: default|python|node|go|rust")
	cmd.Flags().StringArrayVar(&setFlags, "set", nil, "Set a template variable (repeatable: --set key=value)")
	return cmd
}

func initTemplateUsage() string {
	return "Template name, file path, or URL (run 'okdev template list' to see available templates)"
}

func parseSetFlags(flags []string) map[string]string {
	result := make(map[string]string, len(flags))
	for _, flag := range flags {
		parts := strings.SplitN(flag, "=", 2)
		if len(parts) != 2 {
			continue
		}
		result[strings.TrimSpace(parts[0])] = parts[1]
	}
	return result
}

func warnUnknownTemplateSets(w io.Writer, meta *config.TemplateMeta, sets map[string]string) {
	if len(sets) == 0 || meta == nil {
		return
	}
	known := make(map[string]bool, len(meta.Variables))
	for _, v := range meta.Variables {
		known[v.Name] = true
	}
	for name := range sets {
		if !known[name] {
			fmt.Fprintf(w, "Warning: template variable %q is not declared by this template\n", name)
		}
	}
}

func resolveInitTemplateVars(meta *config.TemplateMeta, sets map[string]string, stored map[string]any, nonInteractive, interactive bool, in io.Reader, out io.Writer) (map[string]any, error) {
	if meta == nil || len(meta.Variables) == 0 {
		return map[string]any{}, nil
	}
	if nonInteractive {
		return config.ResolveVariables(meta, sets, stored)
	}

	resolved := make(map[string]any, len(meta.Variables))
	for _, v := range meta.Variables {
		if raw, ok := sets[v.Name]; ok {
			coerced, err := config.CoerceVariableValue(v, raw)
			if err != nil {
				return nil, err
			}
			resolved[v.Name] = coerced
			continue
		}
		if val, ok := stored[v.Name]; ok {
			resolved[v.Name] = val
			continue
		}
		// Don't pre-populate defaults here — let promptTemplateVars
		// show the default as a hint so the user can override it.
	}
	if !interactive {
		return nil, fmt.Errorf("interactive init requires a TTY; rerun with --yes or pass explicit --set values")
	}
	reader := bufio.NewReader(in)
	return promptTemplateVars(reader, out, meta, resolved)
}

func persistTemplateRefIfNeeded(rendered, templateRef string, customVars map[string]any, projectDir string) (string, error) {
	ref := strings.TrimSpace(templateRef)
	if ref == "" {
		ref = "basic"
	}
	if isActualBuiltinBasic(ref, projectDir) && len(customVars) == 0 {
		return rendered, nil
	}

	var doc yamlv3.Node
	if err := yamlv3.Unmarshal([]byte(rendered), &doc); err != nil {
		return "", fmt.Errorf("parse generated config for template metadata: %w", err)
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return "", fmt.Errorf("generated config must be a YAML mapping")
	}
	spec := ensureYAMLMapping(root, "spec")
	templateNode := &yamlv3.Node{Kind: yamlv3.MappingNode}
	setYAMLNode(templateNode, "name", yamlScalar(ref))
	if len(customVars) > 0 {
		var varsNode yamlv3.Node
		if err := varsNode.Encode(customVars); err != nil {
			return "", fmt.Errorf("encode template vars: %w", err)
		}
		setYAMLNode(templateNode, "vars", &varsNode)
	}
	setYAMLNode(spec, "template", templateNode)

	out, err := yamlv3.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("marshal generated config with template metadata: %w", err)
	}
	return string(out), nil
}

func yamlDocumentRoot(doc *yamlv3.Node) *yamlv3.Node {
	if doc.Kind == yamlv3.DocumentNode && len(doc.Content) > 0 {
		return doc.Content[0]
	}
	return doc
}

func ensureYAMLMapping(parent *yamlv3.Node, key string) *yamlv3.Node {
	if existing := findYAMLNode(parent, key); existing != nil && existing.Kind == yamlv3.MappingNode {
		return existing
	}
	node := &yamlv3.Node{Kind: yamlv3.MappingNode}
	setYAMLNode(parent, key, node)
	return node
}

func findYAMLNode(parent *yamlv3.Node, key string) *yamlv3.Node {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			return parent.Content[i+1]
		}
	}
	return nil
}

func setYAMLNode(parent *yamlv3.Node, key string, value *yamlv3.Node) {
	for i := 0; i+1 < len(parent.Content); i += 2 {
		if parent.Content[i].Value == key {
			parent.Content[i+1] = value
			return
		}
	}
	parent.Content = append(parent.Content, yamlScalar(key), value)
}

func yamlScalar(value string) *yamlv3.Node {
	return &yamlv3.Node{Kind: yamlv3.ScalarNode, Value: value}
}

func initProjectDir(configPath string) (string, error) {
	if strings.TrimSpace(configPath) != "" {
		abs, err := filepath.Abs(configPath)
		if err != nil {
			return "", fmt.Errorf("resolve output path %q: %w", configPath, err)
		}
		return config.RootDir(abs), nil
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", fmt.Errorf("get working directory: %w", err)
	}
	return wd, nil
}

func defaultInitTargetPath(vars *config.TemplateVars, meta *config.TemplateMeta, rendered string) string {
	if initUsesWorkloadManifest(vars) || templateDeclaresCompanionFiles(meta) || renderedInitUsesWorkloadManifest(rendered) {
		return config.FolderFile
	}
	return config.DefaultFile
}

func templateDeclaresCompanionFiles(meta *config.TemplateMeta) bool {
	return meta != nil && len(meta.Files) > 0
}

func initUsesWorkloadManifest(vars *config.TemplateVars) bool {
	if vars == nil {
		return false
	}
	switch strings.TrimSpace(vars.WorkloadType) {
	case "job", "pytorchjob", "generic":
		return true
	default:
		return false
	}
}

func renderedInitUsesWorkloadManifest(rendered string) bool {
	if strings.TrimSpace(rendered) == "" {
		return false
	}
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		return false
	}
	return strings.TrimSpace(cfg.Spec.Workload.ManifestPath) != ""
}

func normalizeInitManifestPathForTarget(configPath string, vars *config.TemplateVars, explicit bool) {
	if explicit || !isFolderConfigPath(configPath) {
		return
	}

	switch strings.TrimSpace(vars.WorkloadType) {
	case "job":
		if vars.ManifestPath == ".okdev/job.yaml" {
			vars.ManifestPath = "job.yaml"
		}
	case "pytorchjob":
		if vars.ManifestPath == ".okdev/pytorchjob.yaml" {
			vars.ManifestPath = "pytorchjob.yaml"
		}
	case "generic":
		if strings.TrimSpace(vars.GenericPreset) == "deployment" && vars.ManifestPath == ".okdev/deployment.yaml" {
			vars.ManifestPath = "deployment.yaml"
		}
	}
}

func isFolderConfigPath(configPath string) bool {
	return filepath.Base(configPath) == "okdev.yaml" && filepath.Base(filepath.Dir(configPath)) == ".okdev"
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
			vars.InjectPaths = []string{
				"spec.pytorchReplicaSpecs.Master.template",
				"spec.pytorchReplicaSpecs.Worker.template",
			}
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

func validateRenderedInitConfig(rendered, templateRef string, vars *config.TemplateVars, projectDir string) error {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		return fmt.Errorf("parse generated config: %w", err)
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}
	if vars.WorkloadType != "pod" && !isActualBuiltinBasic(templateRef, projectDir) && strings.TrimSpace(cfg.Spec.Workload.ManifestPath) == "" {
		return fmt.Errorf("custom template must render spec.workload for workload type %q", vars.WorkloadType)
	}
	return nil
}

func scaffoldInitWorkload(configPath, templateRef string, vars *config.TemplateVars, force bool, projectDir string) ([]string, error) {
	if !scaffoldsInitWorkload(templateRef, vars, projectDir) {
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
	target := resolveInitScaffoldFilePath(configPath, vars.ManifestPath)
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

func scaffoldInitTemplateFiles(configPath, templateRef string, meta *config.TemplateMeta, vars *config.TemplateVars, customVars map[string]any, force bool, projectDir string) ([]string, error) {
	if meta == nil || len(meta.Files) == 0 {
		return nil, nil
	}
	var wrote []string
	for _, file := range meta.Files {
		pathTemplate := strings.TrimSpace(file.Path)
		assetRef := strings.TrimSpace(file.Template)
		if pathTemplate == "" {
			return nil, fmt.Errorf("template file path is required")
		}
		if assetRef == "" {
			return nil, fmt.Errorf("template file %q requires template", pathTemplate)
		}
		renderedPath, err := config.RenderTemplateContent("template-file-path", pathTemplate, vars, customVars)
		if err != nil {
			return nil, fmt.Errorf("render template file path %q: %w", pathTemplate, err)
		}
		target := strings.TrimSpace(renderedPath)
		if target == "" {
			return nil, fmt.Errorf("template file path %q rendered empty", pathTemplate)
		}
		target = resolveInitScaffoldFilePath(configPath, target)
		if _, err := os.Stat(target); err == nil && !force {
			continue
		}

		raw, err := config.ResolveTemplateAssetFromDir(context.Background(), templateRef, assetRef, projectDir)
		if err != nil {
			return nil, fmt.Errorf("resolve template file %q: %w", assetRef, err)
		}
		rendered, err := config.RenderTemplateContent(filepath.Base(assetRef), raw, vars, customVars)
		if err != nil {
			return nil, fmt.Errorf("render template file %q: %w", assetRef, err)
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return nil, fmt.Errorf("create template file directory: %w", err)
		}
		if err := os.WriteFile(target, []byte(rendered), 0o644); err != nil {
			return nil, fmt.Errorf("write template file %q: %w", target, err)
		}
		wrote = append(wrote, target)
	}
	return wrote, nil
}

func resolveInitScaffoldFilePath(configPath, path string) string {
	target := strings.TrimSpace(path)
	if filepath.IsAbs(target) {
		return filepath.Clean(target)
	}
	if isFolderConfigPath(configPath) && pathStartsWithDotOkdev(target) {
		return filepath.Clean(filepath.Join(config.RootDir(configPath), target))
	}
	return filepath.Clean(filepath.Join(config.ManifestDir(configPath), target))
}

func pathStartsWithDotOkdev(path string) bool {
	cleaned := filepath.Clean(strings.TrimSpace(path))
	return cleaned == ".okdev" || strings.HasPrefix(cleaned, ".okdev"+string(filepath.Separator))
}

func usesBuiltinBasicTemplate(ref string) bool {
	return strings.TrimSpace(ref) == "" || strings.TrimSpace(ref) == "basic"
}

// isActualBuiltinBasic checks whether the ref resolves to the actual built-in
// basic template, not a project/user template that shadows the name.
func isActualBuiltinBasic(ref, projectDir string) bool {
	if !usesBuiltinBasicTemplate(ref) {
		return false
	}
	// If a project-local or user template shadows "basic", it's not the built-in.
	if names, _ := config.ProjectTemplateNames(projectDir); containsString(names, "basic") {
		return false
	}
	if names, _ := config.UserTemplateNames(); containsString(names, "basic") {
		return false
	}
	return true
}

func scaffoldsInitWorkload(templateRef string, vars *config.TemplateVars, projectDir string) bool {
	if !isActualBuiltinBasic(templateRef, projectDir) {
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

func scaffoldZshFiles(configPath string, vars *config.TemplateVars, force bool, w io.Writer) ([]string, error) {
	if !isZshShellPath(vars.Shell) {
		return nil, nil
	}
	var wrote []string

	zshrcPath := resolveInitScaffoldFilePath(configPath, ".okdev/zshrc")
	if _, err := os.Stat(zshrcPath); err != nil || force {
		content, err := config.RenderEmbeddedTemplate("templates/zshrc.tmpl", vars)
		if err != nil {
			return nil, fmt.Errorf("render zshrc template: %w", err)
		}
		if err := os.MkdirAll(filepath.Dir(zshrcPath), 0o755); err != nil {
			return nil, fmt.Errorf("create zshrc directory: %w", err)
		}
		if err := os.WriteFile(zshrcPath, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write zshrc: %w", err)
		}
		wrote = append(wrote, zshrcPath)
	}

	examplePath := resolveInitScaffoldFilePath(configPath, ".okdev/zsh-setup.example.sh")
	if _, err := os.Stat(examplePath); err != nil || force {
		content, err := config.RenderEmbeddedTemplate("templates/zsh-setup.example.sh.tmpl", vars)
		if err != nil {
			return nil, fmt.Errorf("render zsh-setup example template: %w", err)
		}
		if err := os.WriteFile(examplePath, []byte(content), 0o644); err != nil {
			return nil, fmt.Errorf("write zsh-setup example: %w", err)
		}
		wrote = append(wrote, examplePath)
	}

	if len(wrote) > 0 {
		fmt.Fprintln(w, "Note: spec.ssh.shell affects interactive SSH sessions only.")
		fmt.Fprintln(w, "      zsh must exist in the image or be installed by your lifecycle hook.")
		fmt.Fprintln(w, "      Review .okdev/zsh-setup.example.sh for oh-my-zsh/plugin setup recipes.")
	}

	return wrote, nil
}

func isZshShellPath(shell string) bool {
	return strings.HasSuffix(strings.TrimSpace(shell), "/zsh")
}

func writeInitSTIgnore(configPath string, rendered []byte, templateRef string, stignorePreset string, force bool, projectDirs ...string) (string, bool, error) {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal(rendered, &cfg); err != nil {
		return "", false, fmt.Errorf("parse generated config for .stignore: %w", err)
	}
	cfg.SetDefaults()
	projectDir := config.RootDir(configPath)
	if len(projectDirs) > 0 {
		projectDir = projectDirs[0]
	}
	var patterns []string
	if isActualBuiltinBasic(templateRef, projectDir) {
		patterns = config.BuiltinTemplateLocalIgnores(templateRef)
	}
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
