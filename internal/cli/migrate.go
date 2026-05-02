package cli

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	sigs_yaml "sigs.k8s.io/yaml"
)

func newMigrateCmd(opts *Options) *cobra.Command {
	var dryRun bool
	var noBackup bool
	var templateRef string
	var setFlags []string
	var yes bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a .okdev.yaml config to the latest schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
			}

			if strings.TrimSpace(templateRef) != "" {
				return runMigrateTemplate(cmd, cfgPath, templateRef, setFlags, yes, dryRun, noBackup)
			}

			raw, err := os.ReadFile(cfgPath)
			if err != nil {
				return fmt.Errorf("read config %q: %w", cfgPath, err)
			}

			var doc yaml.Node
			if err := yaml.Unmarshal(raw, &doc); err != nil {
				return fmt.Errorf("parse config %q: %w", cfgPath, err)
			}

			result, err := config.RunMigrations(&doc, config.DefaultMigrations)
			if err != nil {
				return fmt.Errorf("migration failed: %w", err)
			}

			if len(result.Applied) == 0 {
				if err := scaffoldMigrateZshFiles(cfgPath, cmd.OutOrStdout()); err != nil {
					fmt.Fprintf(cmd.OutOrStdout(), "warning: failed to scaffold zsh files: %v\n", err)
				}
				fmt.Fprintln(cmd.OutOrStdout(), "Config is already up to date.")
				return nil
			}

			out, err := yaml.Marshal(&doc)
			if err != nil {
				return fmt.Errorf("marshal migrated config: %w", err)
			}

			w := cmd.OutOrStdout()

			for _, name := range result.Applied {
				fmt.Fprintf(w, "✓ %s\n", name)
			}
			for _, warning := range result.Warnings {
				fmt.Fprintf(w, "  ⚠ %s\n", warning)
			}

			if dryRun {
				fmt.Fprintln(w, "\n--- dry-run output ---")
				_, _ = io.Copy(w, bytes.NewReader(out))
				return nil
			}

			if !noBackup {
				bakPath := cfgPath + ".bak"
				if err := os.WriteFile(bakPath, raw, 0o644); err != nil {
					return fmt.Errorf("write backup %q: %w", bakPath, err)
				}
			}

			if err := os.WriteFile(cfgPath, out, 0o644); err != nil {
				return fmt.Errorf("write migrated config %q: %w", cfgPath, err)
			}

			if err := scaffoldMigrateZshFiles(cfgPath, w); err != nil {
				fmt.Fprintf(w, "warning: failed to scaffold zsh files: %v\n", err)
			}

			fmt.Fprintf(w, "\nWrote migrated config to %s", cfgPath)
			if !noBackup {
				fmt.Fprintf(w, " (backup: %s.bak)", cfgPath)
			}
			fmt.Fprintln(w)
			return nil
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Print migrated config to stdout without writing")
	cmd.Flags().BoolVar(&noBackup, "no-backup", false, "Skip creating a .bak backup file")
	cmd.Flags().StringVar(&templateRef, "template", "", "Re-apply a template with merge semantics")
	cmd.Flags().StringArrayVar(&setFlags, "set", nil, "Set a template variable (repeatable: --set key=value)")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept stored/default values")
	return cmd
}

type migrateTemplateResult struct {
	merged  string
	summary string
	files   []migrateRenderedFile
}

type migrateRenderedFile struct {
	path    string
	content string
}

func runMigrateTemplate(cmd *cobra.Command, cfgPath, templateRef string, setFlags []string, yes, dryRun, noBackup bool) error {
	warnMigrateUnknownSets(cmd.ErrOrStderr(), templateRef, setFlags, config.RootDir(cfgPath))
	result, err := mergeTemplateConfigWithPrompt(cfgPath, templateRef, setFlags, nil, config.RootDir(cfgPath), yes, isTerminalReader(cmd.InOrStdin()), cmd.InOrStdin(), cmd.OutOrStdout())
	if err != nil {
		return err
	}

	w := cmd.OutOrStdout()
	fmt.Fprintln(w, result.summary)
	if dryRun {
		fmt.Fprintln(w, "\n--- dry-run output ---")
		fmt.Fprint(w, result.merged)
		if len(result.files) > 0 {
			fmt.Fprintln(w, "\nWould rewrite companion files:")
			for _, file := range result.files {
				fmt.Fprintf(w, "- %s\n", file.path)
			}
		}
		return nil
	}
	if !yes {
		ok, err := promptMigrateApply(cmd.InOrStdin(), w)
		if err != nil {
			return err
		}
		if !ok {
			fmt.Fprintln(w, "Aborted.")
			return nil
		}
	}

	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}

	// Write all backups first so even a partial write failure still leaves a
	// recoverable snapshot of every file we intend to modify.
	if !noBackup {
		bakPath := cfgPath + ".bak"
		if err := os.WriteFile(bakPath, raw, 0o644); err != nil {
			return fmt.Errorf("write backup %q: %w", bakPath, err)
		}
		for _, file := range result.files {
			existing, err := os.ReadFile(file.path)
			if err != nil {
				if os.IsNotExist(err) {
					continue
				}
				return fmt.Errorf("read existing companion file %q: %w", file.path, err)
			}
			bakPath := file.path + ".bak"
			if err := os.WriteFile(bakPath, existing, 0o644); err != nil {
				return fmt.Errorf("write backup %q: %w", bakPath, err)
			}
		}
	}

	if err := os.WriteFile(cfgPath, []byte(result.merged), 0o644); err != nil {
		return fmt.Errorf("write migrated config %q: %w", cfgPath, err)
	}
	for _, file := range result.files {
		if err := os.MkdirAll(filepath.Dir(file.path), 0o755); err != nil {
			return fmt.Errorf("create parent directory for %q: %w", file.path, err)
		}
		if err := os.WriteFile(file.path, []byte(file.content), 0o644); err != nil {
			return fmt.Errorf("write migrated companion file %q: %w", file.path, err)
		}
	}
	if err := scaffoldMigrateZshFiles(cfgPath, w); err != nil {
		fmt.Fprintf(w, "warning: failed to scaffold zsh files: %v\n", err)
	}

	fmt.Fprintf(w, "Wrote migrated config to %s", cfgPath)
	if !noBackup {
		fmt.Fprintf(w, " (backup: %s.bak)", cfgPath)
	}
	fmt.Fprintln(w)
	for _, file := range result.files {
		fmt.Fprintf(w, "Rewrote companion file %s", file.path)
		if !noBackup {
			fmt.Fprintf(w, " (backup: %s.bak)", file.path)
		}
		fmt.Fprintln(w)
	}
	return nil
}

func promptMigrateApply(in io.Reader, out io.Writer) (bool, error) {
	if _, err := fmt.Fprint(out, "Apply changes? [y/N] "); err != nil {
		return false, err
	}
	line, err := bufio.NewReader(in).ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}

func mergeTemplateConfig(cfgPath, templateRef string, setFlags []string, storedVars map[string]any, projectDir string, nonInteractive bool) (*migrateTemplateResult, error) {
	return mergeTemplateConfigWithPrompt(cfgPath, templateRef, setFlags, storedVars, projectDir, nonInteractive, false, nil, io.Discard)
}

func mergeTemplateConfigWithPrompt(cfgPath, templateRef string, setFlags []string, storedVars map[string]any, projectDir string, nonInteractive bool, interactive bool, in io.Reader, promptOut io.Writer) (*migrateTemplateResult, error) {
	existingRaw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil, fmt.Errorf("read existing config: %w", err)
	}
	var existingCfg config.DevEnvironment
	if err := sigs_yaml.Unmarshal(existingRaw, &existingCfg); err != nil {
		return nil, fmt.Errorf("parse existing config: %w", err)
	}
	existingCfg.SetDefaults()
	if storedVars == nil && existingCfg.Spec.Template != nil {
		storedVars = existingCfg.Spec.Template.Vars
	}

	rawTemplate, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
	if err != nil {
		return nil, err
	}
	meta, _, err := config.ParseFrontmatter(rawTemplate)
	if err != nil {
		return nil, err
	}
	sets := parseSetFlags(setFlags)
	customVars, err := resolveInitTemplateVars(meta, sets, storedVars, nonInteractive, interactive, in, promptOut)
	if err != nil {
		return nil, err
	}

	vars := templateVarsForMigrate(&existingCfg, cfgPath)
	rendered, err := config.RenderTemplateWithVars(context.Background(), templateRef, vars, customVars, projectDir)
	if err != nil {
		return nil, err
	}

	var templateMap map[string]any
	if err := sigs_yaml.Unmarshal([]byte(rendered), &templateMap); err != nil {
		return nil, fmt.Errorf("parse rendered template config: %w", err)
	}
	var existingMap map[string]any
	if err := sigs_yaml.Unmarshal(existingRaw, &existingMap); err != nil {
		return nil, fmt.Errorf("parse existing config map: %w", err)
	}

	merged, preserved := mergeMaps(templateMap, existingMap, "")
	spec, ok := merged["spec"].(map[string]any)
	if !ok {
		spec = map[string]any{}
		merged["spec"] = spec
	}
	templateBlock := map[string]any{"name": strings.TrimSpace(templateRef)}
	if len(customVars) > 0 {
		templateBlock["vars"] = customVars
	}
	spec["template"] = templateBlock

	out, err := sigs_yaml.Marshal(merged)
	if err != nil {
		return nil, fmt.Errorf("marshal merged config: %w", err)
	}
	var validationCfg config.DevEnvironment
	if err := sigs_yaml.Unmarshal(out, &validationCfg); err != nil {
		return nil, fmt.Errorf("parse merged config for validation: %w", err)
	}
	validationCfg.SetDefaults()
	if err := validationCfg.Validate(); err != nil {
		return nil, fmt.Errorf("merged config is invalid: %w", err)
	}
	files, err := renderMigrateTemplateFiles(cfgPath, templateRef, meta, vars, customVars, projectDir)
	if err != nil {
		return nil, err
	}
	summary := "Template migration summary: added template fields and preserved existing values."
	if preserved > 0 {
		summary = fmt.Sprintf("Template migration summary: preserved %d existing value(s); added missing template fields.\nNote: spec.template.vars records the new variable values, but preserved config fields retain their prior values.", preserved)
	}
	if len(files) > 0 {
		summary = fmt.Sprintf("%s\nWill regenerate %d companion file(s); any local edits to those files will be overwritten.", summary, len(files))
	}
	return &migrateTemplateResult{merged: string(out), summary: summary, files: files}, nil
}

func warnMigrateUnknownSets(w io.Writer, templateRef string, setFlags []string, projectDir string) {
	if len(setFlags) == 0 {
		return
	}
	raw, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
	if err != nil {
		return
	}
	meta, _, err := config.ParseFrontmatter(raw)
	if err != nil {
		return
	}
	warnUnknownTemplateSets(w, meta, parseSetFlags(setFlags))
}

func mergeMaps(base, overlay map[string]any, path string) (map[string]any, int) {
	out := make(map[string]any, len(base)+len(overlay))
	for key, val := range base {
		out[key] = val
	}
	preserved := 0
	for key, overlayVal := range overlay {
		childPath := key
		if path != "" {
			childPath = path + "." + key
		}
		if baseVal, ok := out[key]; ok {
			baseMap, baseIsMap := baseVal.(map[string]any)
			overlayMap, overlayIsMap := overlayVal.(map[string]any)
			if baseIsMap && overlayIsMap {
				merged, count := mergeMaps(baseMap, overlayMap, childPath)
				out[key] = merged
				preserved += count
				continue
			}
			if !reflect.DeepEqual(baseVal, overlayVal) {
				preserved++
			}
		}
		out[key] = overlayVal
	}
	return out, preserved
}

func renderMigrateTemplateFiles(cfgPath, templateRef string, meta *config.TemplateMeta, vars *config.TemplateVars, customVars map[string]any, projectDir string) ([]migrateRenderedFile, error) {
	if meta == nil || len(meta.Files) == 0 {
		return nil, nil
	}
	files := make([]migrateRenderedFile, 0, len(meta.Files))
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
		if strings.TrimSpace(renderedPath) == "" {
			return nil, fmt.Errorf("template file path %q rendered empty", pathTemplate)
		}
		target := resolveInitScaffoldFilePath(cfgPath, renderedPath)
		raw, err := config.ResolveTemplateAssetFromDir(context.Background(), templateRef, assetRef, projectDir)
		if err != nil {
			return nil, fmt.Errorf("resolve template file %q: %w", assetRef, err)
		}
		rendered, err := config.RenderTemplateContent(filepath.Base(assetRef), raw, vars, customVars)
		if err != nil {
			return nil, fmt.Errorf("render template file %q: %w", assetRef, err)
		}
		files = append(files, migrateRenderedFile{path: target, content: rendered})
	}
	return files, nil
}

func templateVarsForMigrate(cfg *config.DevEnvironment, cfgPath string) *config.TemplateVars {
	vars := config.NewTemplateVars()
	if cfg == nil {
		return vars
	}

	if name := strings.TrimSpace(cfg.Metadata.Name); name != "" {
		vars.Name = name
	}
	if namespace := strings.TrimSpace(cfg.Spec.Namespace); namespace != "" {
		vars.Namespace = namespace
	}
	if kubeContext := strings.TrimSpace(cfg.Spec.KubeContext); kubeContext != "" {
		vars.KubeContext = kubeContext
	}
	if workloadType := strings.TrimSpace(cfg.Spec.Workload.Type); workloadType != "" {
		vars.WorkloadType = workloadType
	}
	if manifestPath := strings.TrimSpace(cfg.Spec.Workload.ManifestPath); manifestPath != "" {
		vars.ManifestPath = manifestPath
	}
	if attachContainer := strings.TrimSpace(cfg.Spec.Workload.Attach.Container); attachContainer != "" {
		vars.AttachContainer = attachContainer
	}
	if sshUser := strings.TrimSpace(cfg.Spec.SSH.User); sshUser != "" {
		vars.SSHUser = sshUser
	}
	if shell := strings.TrimSpace(cfg.Spec.SSH.Shell); shell != "" {
		vars.Shell = shell
	}
	if sidecarImage := strings.TrimSpace(cfg.Spec.Sidecar.Image); sidecarImage != "" {
		vars.SidecarImage = sidecarImage
	}
	vars.TTLHours = cfg.Spec.Session.TTLHours

	if len(cfg.Spec.Workload.Inject) > 0 {
		vars.InjectPaths = make([]string, 0, len(cfg.Spec.Workload.Inject))
		for _, inject := range cfg.Spec.Workload.Inject {
			if path := strings.TrimSpace(inject.Path); path != "" {
				vars.InjectPaths = append(vars.InjectPaths, path)
			}
		}
	}
	if len(cfg.Spec.Ports) > 0 {
		vars.Ports = make([]config.PortVar, 0, len(cfg.Spec.Ports))
		for _, port := range cfg.Spec.Ports {
			vars.Ports = append(vars.Ports, config.PortVar{
				Name:   port.Name,
				Local:  port.Local,
				Remote: port.Remote,
			})
		}
	}

	defaultRemote := cfg.EffectiveWorkspaceMountPath(cfgPath)
	if pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, defaultRemote); err == nil && len(pairs) > 0 {
		if local := strings.TrimSpace(pairs[0].Local); local != "" {
			vars.SyncLocal = local
		}
		if remote := strings.TrimSpace(pairs[0].Remote); remote != "" {
			vars.SyncRemote = remote
		}
	}

	devContainer := findTemplateContainer(cfg.Spec.PodTemplate.Spec.Containers, "dev")
	if devContainer != nil {
		if image := strings.TrimSpace(devContainer.Image); image != "" {
			vars.DevImage = image
		}
		setTemplateResourceStrings(&vars.DevCPURequest, &vars.DevMemoryRequest, devContainer.Resources.Requests)
		setTemplateResourceStrings(&vars.DevCPULimit, &vars.DevMemoryLimit, devContainer.Resources.Limits)
	}
	setTemplateResourceStrings(&vars.SidecarCPU, &vars.SidecarMemory, cfg.Spec.Sidecar.Resources.Requests)
	if strings.TrimSpace(vars.SidecarCPU) == "" || strings.TrimSpace(vars.SidecarMemory) == "" {
		setTemplateResourceStrings(&vars.SidecarCPU, &vars.SidecarMemory, cfg.Spec.Sidecar.Resources.Limits)
	}

	return vars
}

func findTemplateContainer(containers []corev1.Container, name string) *corev1.Container {
	for i := range containers {
		if strings.TrimSpace(containers[i].Name) == name {
			return &containers[i]
		}
	}
	if len(containers) == 0 {
		return nil
	}
	return &containers[0]
}

func setTemplateResourceStrings(cpuOut, memoryOut *string, resources corev1.ResourceList) {
	if resources == nil {
		return
	}
	if cpuOut != nil && strings.TrimSpace(*cpuOut) == "" {
		if quantity, ok := resources[corev1.ResourceCPU]; ok {
			*cpuOut = quantity.String()
		}
	}
	if memoryOut != nil && strings.TrimSpace(*memoryOut) == "" {
		if quantity, ok := resources[corev1.ResourceMemory]; ok {
			*memoryOut = quantity.String()
		}
	}
}

func scaffoldMigrateZshFiles(cfgPath string, w io.Writer) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return nil
	}
	var cfg config.DevEnvironment
	if err := sigs_yaml.Unmarshal(raw, &cfg); err != nil {
		return nil
	}
	shell := strings.TrimSpace(cfg.Spec.SSH.Shell)
	if !strings.HasSuffix(shell, "/zsh") {
		return nil
	}

	okdevDir := filepath.Dir(cfgPath)
	if filepath.Base(okdevDir) != ".okdev" {
		okdevDir = filepath.Join(filepath.Dir(cfgPath), ".okdev")
	}

	vars := config.NewTemplateVars()

	zshrcPath := filepath.Join(okdevDir, "zshrc")
	if _, err := os.Stat(zshrcPath); os.IsNotExist(err) {
		content, err := config.RenderEmbeddedTemplate("templates/zshrc.tmpl", vars)
		if err != nil {
			return fmt.Errorf("render zshrc template: %w", err)
		}
		if err := os.MkdirAll(okdevDir, 0o755); err != nil {
			return fmt.Errorf("create .okdev directory: %w", err)
		}
		if err := os.WriteFile(zshrcPath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write zshrc: %w", err)
		}
		fmt.Fprintf(w, "Wrote %s\n", zshrcPath)
	}

	examplePath := filepath.Join(okdevDir, "zsh-setup.example.sh")
	if _, err := os.Stat(examplePath); os.IsNotExist(err) {
		content, err := config.RenderEmbeddedTemplate("templates/zsh-setup.example.sh.tmpl", vars)
		if err != nil {
			return fmt.Errorf("render zsh-setup example: %w", err)
		}
		if err := os.WriteFile(examplePath, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write zsh-setup example: %w", err)
		}
		fmt.Fprintf(w, "Wrote %s\n", examplePath)
	}

	fmt.Fprintln(w, "Note: spec.ssh.shell affects interactive SSH sessions only.")
	fmt.Fprintln(w, "      zsh must exist in the image or be installed by your lifecycle hook.")
	fmt.Fprintln(w, "      Review .okdev/zsh-setup.example.sh for oh-my-zsh/plugin setup recipes.")

	return nil
}
