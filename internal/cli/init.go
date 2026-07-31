package cli

import (
	"bufio"
	"bytes"
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
	var workloadName string
	var manifestPath string
	var injectPaths []string
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
			if err := rejectRemovedWorkloadFlags(cmd); err != nil {
				return err
			}
			vars := config.NewTemplateVars()
			overrides := InitOverrides{
				Name:         nameOverride,
				Namespace:    nsOverride,
				KubeContext:  contextOverride,
				ManifestPath: manifestPath,
				InjectPaths:  injectPaths,
				DevImage:     devImageOverride,
				SidecarImage: sidecarImageOverride,
				SyncLocal:    syncLocalOverride,
				SyncRemote:   syncRemoteOverride,
				SSHUser:      sshUserOverride,
				Shell:        shellOverride,
			}
			applyOverrides(vars, overrides)

			// Adding a workload to a project that already has a config is a
			// different operation from creating one: it never prompts, never
			// renders the config template, and never touches project-level
			// settings. Decide before any of that machinery runs.
			existing, err := existingConfigPath(opts)
			if err != nil {
				return err
			}
			inv := initInvocation{
				ConfigExists:    existing != "",
				WorkloadName:    workloadName,
				Force:           force,
				ProjectFlagsSet: changedProjectFlags(cmd),
			}
			if err := validateInitInvocation(inv); err != nil {
				return err
			}
			if initAdditiveMode(inv) {
				return runInitAddWorkload(cmd, existing, vars, workloadName, templateRef)
			}

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
			rendered, err = config.RenderTemplateContent("okdev", body, vars, customVars)
			if err != nil {
				return err
			}
			rendered, err = persistTemplateRefIfNeeded(rendered, templateRef, customVars, projectDir)
			if err != nil {
				return err
			}

			// A template written before "every workload is a manifest" either
			// defines the pod inline under spec.podTemplate, declares a pod
			// workload with no manifest, or says nothing about workloads at all
			// — pod was the default and okdev filled the rest in. All three are
			// what `okdev migrate` resolves for an existing config, so run the
			// same resolution here rather than inventing a second one. Telling
			// the user to run migrate would not work: no config exists yet.
			extraction, err := planPodTemplateExtraction(abs, []byte(rendered))
			if err != nil {
				return err
			}
			if extraction.Applied {
				rendered = string(extraction.ConfigBytes)
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
			// --stignore-preset wins, then repo detection, then whatever the
			// template declared.
			resolvedPreset := strings.TrimSpace(stignorePreset)
			if resolvedPreset == "" {
				resolvedPreset = detectSTIgnorePreset(config.RootDir(abs))
			}
			if resolvedPreset == "" && meta != nil {
				resolvedPreset = strings.TrimSpace(meta.StignorePreset)
			}
			stignorePath, wroteSTIgnore, err := writeInitSTIgnore(abs, []byte(rendered), resolvedPreset)
			if err != nil {
				return err
			}
			scaffolded, err := scaffoldInitTemplateFiles(abs, templateRef, meta, vars, customVars, force, projectDir)
			if err != nil {
				return err
			}
			// ...and the manifest that resolution now points at.
			for _, m := range extraction.Manifests {
				if _, err := os.Stat(m.Target); err == nil && !force {
					continue
				}
				if err := os.MkdirAll(filepath.Dir(m.Target), 0o755); err != nil {
					return fmt.Errorf("create manifest directory: %w", err)
				}
				if err := os.WriteFile(m.Target, m.Bytes, 0o644); err != nil {
					return fmt.Errorf("write scaffolded manifest %q: %w", m.Target, err)
				}
				scaffolded = append(scaffolded, m.Target)
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
			switch {
			case wroteSTIgnore:
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", stignorePath)
			case stignorePath != "":
				fmt.Fprintf(cmd.OutOrStdout(), "Kept existing %s\n", stignorePath)
			}
			for _, path := range scaffolded {
				fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", path)
			}
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().StringVar(&templateRef, "template", "", initTemplateUsage())
	// Kept only so the removal can name a replacement instead of cobra saying
	// "unknown flag". Hidden, so they do not appear as options.
	cmd.Flags().String("workload", "", "removed; use --template <name>")
	cmd.Flags().String("generic-preset", "", "removed; use --template deployment")
	_ = cmd.Flags().MarkHidden("workload")
	_ = cmd.Flags().MarkHidden("generic-preset")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept all defaults")
	cmd.Flags().StringVar(&nameOverride, "name", "", "Environment name")
	cmd.Flags().StringVar(&nsOverride, "namespace", "", "Namespace")
	cmd.Flags().StringVar(&contextOverride, "context", "", "Kubeconfig context (defaults to active context)")
	cmd.Flags().StringVar(&workloadName, "workload-name", "", "Name for the workload being added to an existing config")
	cmd.Flags().StringVar(&manifestPath, "manifest-path", "", "Path to workload manifest")
	cmd.Flags().StringArrayVar(&injectPaths, "inject-path", nil, "Workload inject path (repeatable)")
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

	// yaml.v3 defaults to 4-space indent; the templates are written with 2, and
	// this rewrite is now on the path of every init rather than only templates
	// with variables, so matching them keeps generated configs looking hand-written.
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return "", fmt.Errorf("render config with template metadata: %w", err)
	}
	if err := enc.Close(); err != nil {
		return "", fmt.Errorf("render config with template metadata: %w", err)
	}
	out, err := buf.Bytes(), error(nil)
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

// defaultInitTargetPath is always the folder config. A flat .okdev.yaml puts
// ManifestDir at the project root, so every manifest added to the project
// later would land there; the folder gives manifests a home from the start.
func defaultInitTargetPath(*config.TemplateVars, *config.TemplateMeta, string) string {
	return config.FolderFile
}

func isFolderConfigPath(configPath string) bool {
	return filepath.Base(configPath) == "okdev.yaml" && filepath.Base(filepath.Dir(configPath)) == ".okdev"
}

// applyWorkloadDefaults fills in only what a template cannot state for itself.
// Manifest paths and inject paths used to be defaulted per workload type here;
// each built-in template writes its own now.
func applyWorkloadDefaults(vars *config.TemplateVars) {
	if strings.TrimSpace(vars.AttachContainer) == "" {
		vars.AttachContainer = "dev"
	}
}

// validateInitWorkloadVars checks the flags that only the "generic" shape uses.
// Which shape is in play is now the template's business, so this cannot know
// whether they are required — the rendered config is checked instead, in
// validateRenderedInitConfig.
func validateInitWorkloadVars(vars *config.TemplateVars) error {
	if len(vars.InjectPaths) > 0 && strings.TrimSpace(vars.ManifestPath) == "" {
		return fmt.Errorf("--inject-path needs --manifest-path: inject paths address a manifest")
	}
	return nil
}

func validateRenderedInitConfig(rendered, templateRef string, vars *config.TemplateVars, projectDir string) error {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal([]byte(rendered), &cfg); err != nil {
		return fmt.Errorf("parse generated config: %w", err)
	}
	cfg.SetDefaults()
	// SetDefaults desugars spec.workload into spec.workloads but does not
	// collapse the other way, so a config that declares profiles leaves the
	// singular empty. Select the effective one, as every command does.
	if err := cfg.SelectWorkload(""); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}
	// Before Validate, so a template that declared a workload without saying
	// where its manifest is gets told exactly that, rather than the generic
	// "manifestPath is required" it would otherwise trip on. A template that
	// declared no workload at all is a different case, handled by the caller.
	if strings.TrimSpace(cfg.Spec.Workload.ManifestPath) == "" {
		return fmt.Errorf("template %q rendered no spec.workload.manifestPath.\n\n%s",
			templateName(templateRef), missingWorkloadHelp(templateName(templateRef), projectDir))
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("generated config is invalid: %w", err)
	}
	return nil
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

// writeInitSTIgnore writes the starter ignore file for the generated project.
//
// The preset is whatever the caller resolved — the template's own
// `stignorePreset` frontmatter, or --stignore-preset overriding it. Nothing here
// keys off the template's *name*, which is what used to force okdev to check
// whether a name resolved to the real built-in before applying built-in-only
// behavior.
func writeInitSTIgnore(configPath string, rendered []byte, stignorePreset string) (string, bool, error) {
	var cfg config.DevEnvironment
	if err := yaml.Unmarshal(rendered, &cfg); err != nil {
		return "", false, fmt.Errorf("parse generated config for .stignore: %w", err)
	}
	cfg.SetDefaults()
	var patterns []string
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
	// Only pairs[0].Local is used below, so the default remote is immaterial —
	// and the workload manifest this would otherwise be read from has not been
	// scaffolded yet at this point in init.
	pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, config.DefaultWorkspacePath)
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
	// An existing .stignore is never replaced, not even with --force. It
	// accumulates hand-written rules — the dataset directory someone excluded
	// after a slow first sync — and --force is about regenerating the config,
	// not about discarding that.
	stignorePath := filepath.Join(localRoot, ".stignore")
	if _, err := os.Stat(stignorePath); err == nil {
		return stignorePath, false, nil
	}
	if err := os.WriteFile(stignorePath, []byte(content), 0o644); err != nil {
		return "", false, fmt.Errorf("write .stignore %q: %w", stignorePath, err)
	}
	return stignorePath, true, nil
}

// templateName is the ref as the user typed it, or the default when omitted.
func templateName(ref string) string {
	if r := strings.TrimSpace(ref); r != "" {
		return r
	}
	return "basic"
}

// rejectRemovedWorkloadFlags turns --workload / --generic-preset into an error
// naming the template that replaced them.
//
// Silently ignoring them would be worse than removing them: someone asking for
// a Job would get a pod and only find out on the cluster.
func rejectRemovedWorkloadFlags(cmd *cobra.Command) error {
	preset := ""
	if f := cmd.Flags().Lookup("generic-preset"); f != nil && f.Changed {
		preset = strings.TrimSpace(f.Value.String())
	}
	f := cmd.Flags().Lookup("workload")
	if f == nil || !f.Changed {
		if preset != "" {
			return fmt.Errorf("--generic-preset is removed; use --template %s instead\n%s", preset, availableTemplatesHint())
		}
		return nil
	}
	replacement := strings.TrimSpace(f.Value.String())
	if replacement == "generic" && preset != "" {
		replacement = preset
	}
	return fmt.Errorf("--workload is removed; use --template %s instead\n%s", replacement, availableTemplatesHint())
}

func availableTemplatesHint() string {
	return "(available: " + strings.Join(config.BuiltinTemplateNames(), ", ") + "; run `okdev template list` to include your own)"
}

// missingWorkloadHelp explains how to fix a template that renders no workload.
//
// Pod used to be exempt: it was the default shape, okdev filled the manifest
// path in, and a template could omit spec.workload entirely. Both of those went
// away — every workload is a manifest now — so a template written before that
// fails here, and the reader is most often looking at a template of their own
// that shadows a built-in name. Naming that case is the whole point: telling
// them to "pass --manifest-path" would send them to fix the wrong thing.
func missingWorkloadHelp(name, projectDir string) string {
	var b strings.Builder
	if shadowed := shadowedBuiltinLocation(name, projectDir); shadowed != "" {
		fmt.Fprintf(&b, "%s shadows the built-in %q template, and okdev used it as written.\n\n", shadowed, name)
	}
	b.WriteString(`Every workload is a manifest file, so a template has to render a workload
block and ship the manifest it points at:

  # in the template body
  spec:
    workload:
      type: pod
      manifestPath: pod.yaml

  # in the template frontmatter
  files:
    - path: .okdev/pod.yaml
      template: manifests/pod.yaml.tmpl

Or drop the shadowing template to use the built-in, and run
` + "`okdev template list --all`" + ` to see what is shadowing what.`)
	return b.String()
}

// shadowedBuiltinLocation reports where a template shadowing a built-in name
// lives, so the message can point at the file to edit.
func shadowedBuiltinLocation(name, projectDir string) string {
	if !containsString(config.BuiltinTemplateNames(), name) {
		return ""
	}
	if names, _ := config.ProjectTemplateNames(projectDir); containsString(names, name) {
		return filepath.Join(projectDir, ".okdev", "templates", name+".yaml.tmpl")
	}
	if names, _ := config.UserTemplateNames(); containsString(names, name) {
		if dir, err := os.UserHomeDir(); err == nil {
			return filepath.Join(dir, ".okdev", "templates", name+".yaml.tmpl")
		}
		return "~/.okdev/templates/" + name + ".yaml.tmpl"
	}
	return ""
}
