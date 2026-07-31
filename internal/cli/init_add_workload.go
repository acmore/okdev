package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// initInvocation is what the user asked for, reduced to the facts that decide
// whether `okdev init` creates a config or appends a workload to one.
type initInvocation struct {
	ConfigExists bool
	WorkloadName string
	Force        bool
	// ProjectFlagsSet names the project-level flags the user actually passed
	// (Cobra's Changed), e.g. "namespace". They configure a project at
	// creation and have no meaning when appending to an existing one.
	ProjectFlagsSet []string
}

// initAdditiveMode reports whether this run appends a workload to an existing
// config instead of creating one.
func initAdditiveMode(inv initInvocation) bool {
	return inv.ConfigExists && !inv.Force && strings.TrimSpace(inv.WorkloadName) != ""
}

// validateInitInvocation rejects incoherent flag combinations before any I/O,
// so a refusal always leaves the project byte-identical.
func validateInitInvocation(inv initInvocation) error {
	name := strings.TrimSpace(inv.WorkloadName)

	if !inv.ConfigExists {
		if name != "" {
			return fmt.Errorf("--workload-name applies when adding a workload to an existing config; a new config's first workload is named %q", "default")
		}
		return nil
	}

	if inv.Force {
		if name != "" {
			// --force rewrites the whole config; appending keeps it. Together
			// they state opposite intents, and silently picking one discards
			// the other.
			return fmt.Errorf("--force rewrites the whole config and --workload-name appends to it; pass only one")
		}
		return nil
	}

	if name == "" {
		return fmt.Errorf("config already exists; pass --workload-name to add a workload to it, or --force to rewrite it")
	}

	if len(inv.ProjectFlagsSet) > 0 {
		return fmt.Errorf("--%s configures a project at creation and cannot be changed by adding a workload; edit the config file instead",
			strings.Join(inv.ProjectFlagsSet, ", --"))
	}
	return nil
}

// additiveManifestPath is where a newly declared workload's manifest goes,
// expressed relative to the config the way ResolveWorkloadManifestPath expects.
// Manifests always live in .okdev/: for a folder config that is the config's
// own directory, for a pre-existing flat config it is a sibling folder.
func additiveManifestPath(cfgPath, workloadName string) string {
	if isFolderConfigPath(cfgPath) {
		return workloadName + ".yaml"
	}
	return filepath.Join(".okdev", workloadName+".yaml")
}

// workloadAddition is a complete, not-yet-written change set.
type workloadAddition struct {
	ConfigBytes    []byte
	ManifestTarget string
	ManifestBytes  []byte
}

// planWorkloadAddition computes everything the additive path will write and
// proves the result is valid, without touching the filesystem.
func planWorkloadAddition(cfgPath string, raw []byte, cfg *config.DevEnvironment, vars *config.TemplateVars, workloadName, templateRef, projectDir string) (*workloadAddition, error) {
	name := strings.TrimSpace(workloadName)
	if name == "" {
		return nil, fmt.Errorf("--workload-name is required")
	}
	for _, existing := range cfg.WorkloadProfileNames() {
		if existing == name {
			return nil, fmt.Errorf("workload %q is already declared; existing workloads: %s",
				name, strings.Join(cfg.WorkloadProfileNames(), ", "))
		}
	}

	applyWorkloadDefaults(vars)

	// The template says what shape to add. Only its workload block is taken:
	// namespace, sync, ssh and the rest belong to the config being extended,
	// and additive mode changes nothing project-wide.
	rawTemplate, err := config.ResolveTemplateFromDir(context.Background(), templateRef, projectDir)
	if err != nil {
		return nil, err
	}
	meta, body, err := config.ParseFrontmatter(rawTemplate)
	if err != nil {
		return nil, err
	}
	rendered, err := config.RenderTemplateContent("okdev", body, vars, nil)
	if err != nil {
		return nil, err
	}
	var shape config.DevEnvironment
	if err := yaml.Unmarshal([]byte(rendered), &shape); err != nil {
		return nil, fmt.Errorf("parse template %q: %w", templateName(templateRef), err)
	}
	declared := shape.Spec.Workload
	if len(shape.Spec.Workloads) > 0 {
		p := shape.Spec.Workloads[0]
		declared = config.WorkloadSpec{Type: p.Type, ManifestPath: p.ManifestPath, Inject: p.Inject, Attach: p.Attach}
	}
	if strings.TrimSpace(declared.ManifestPath) == "" {
		return nil, fmt.Errorf("template %q rendered no spec.workload.manifestPath; "+
			"pass --manifest-path or use a template that ships one", templateName(templateRef))
	}

	profile := config.WorkloadProfile{
		Name:         name,
		Type:         declared.Type,
		ManifestPath: declared.ManifestPath,
		Inject:       declared.Inject,
		Attach:       declared.Attach,
	}

	// The template's own manifest is the files: entry matching what it declared
	// as manifestPath. It is written as <workload-name>.yaml so two workloads of
	// the same shape never collide on one file.
	var manifestBytes []byte
	if asset := templateWorkloadAsset(meta, declared.ManifestPath); asset != "" {
		raw, err := config.ResolveTemplateAssetFromDir(context.Background(), templateRef, asset, projectDir)
		if err != nil {
			return nil, fmt.Errorf("resolve template file %q: %w", asset, err)
		}
		out, err := config.RenderTemplateContent(filepath.Base(asset), raw, vars, nil)
		if err != nil {
			return nil, fmt.Errorf("render template file %q: %w", asset, err)
		}
		manifestBytes = []byte(out)
		profile.ManifestPath = additiveManifestPath(cfgPath, name)
	}

	configBytes, err := appendWorkloadProfileToConfigBytes(raw, profile)
	if err != nil {
		return nil, err
	}

	// The guarantee: nothing is written unless the edited config decodes,
	// defaults and validates. LoadFromBytes does all three, so a successful
	// call is the proof.
	if _, _, err := config.LoadFromBytes(configBytes, cfgPath); err != nil {
		return nil, fmt.Errorf("adding workload %q would make the config invalid: %w", name, err)
	}

	add := &workloadAddition{ConfigBytes: configBytes, ManifestBytes: manifestBytes}
	if len(manifestBytes) > 0 {
		add.ManifestTarget = workload.ResolveManifestPath(cfgPath, profile.ManifestPath)
	}
	return add, nil
}

// templateWorkloadAsset finds the declared file that is the workload's manifest:
// the one whose path matches what the template rendered as manifestPath.
func templateWorkloadAsset(meta *config.TemplateMeta, manifestPath string) string {
	if meta == nil {
		return ""
	}
	want := filepath.Base(strings.TrimSpace(manifestPath))
	for _, f := range meta.Files {
		if filepath.Base(strings.TrimSpace(f.Path)) == want {
			return strings.TrimSpace(f.Template)
		}
	}
	return ""
}

// appendWorkloadProfileToConfigBytes adds a profile to spec.workloads in place.
//
// It edits the YAML node tree rather than round-tripping through the config
// struct: a full unmarshal/marshal would silently strip the user's comments and
// reorder their keys.
func appendWorkloadProfileToConfigBytes(raw []byte, p config.WorkloadProfile) ([]byte, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	spec := ensureYAMLMapping(root, "spec")

	workloads := findYAMLNode(spec, "workloads")
	if workloads == nil {
		workloads = &yamlv3.Node{Kind: yamlv3.SequenceNode}
		// Materialize the workload this config already runs as the first
		// profile, or declaring a second one silently drops the first.
		//
		// It is materialized even when spec.workload is absent: `okdev init`
		// omits that block entirely for pod configs, since pod is the default,
		// and the pod workload is no less real for being implicit.
		existing := &yamlv3.Node{Kind: yamlv3.MappingNode}
		setYAMLNode(existing, "name", yamlScalar(config.DefaultWorkloadProfileName))
		if legacy := findYAMLNode(spec, "workload"); legacy != nil {
			for _, key := range []string{"type", "manifestPath", "inject", "attach"} {
				if v := findYAMLNode(legacy, key); v != nil {
					setYAMLNode(existing, key, v)
				}
			}
		}
		if findYAMLNode(existing, "type") == nil {
			setYAMLNode(existing, "type", yamlScalar(workload.TypePod))
		}
		workloads.Content = append(workloads.Content, existing)
		setYAMLNode(spec, "workloads", workloads)
	}
	if workloads.Kind != yamlv3.SequenceNode {
		return nil, fmt.Errorf("spec.workloads is not a list")
	}
	for _, entry := range workloads.Content {
		if name := findYAMLNode(entry, "name"); name != nil && strings.TrimSpace(name.Value) == strings.TrimSpace(p.Name) {
			return nil, fmt.Errorf("workload %q already exists in the config", p.Name)
		}
	}

	entry := &yamlv3.Node{Kind: yamlv3.MappingNode}
	setYAMLNode(entry, "name", yamlScalar(p.Name))
	setYAMLNode(entry, "type", yamlScalar(p.Type))
	if strings.TrimSpace(p.ManifestPath) != "" {
		setYAMLNode(entry, "manifestPath", yamlScalar(p.ManifestPath))
	}
	if len(p.Inject) > 0 {
		injects := &yamlv3.Node{Kind: yamlv3.SequenceNode}
		for _, in := range p.Inject {
			node := &yamlv3.Node{Kind: yamlv3.MappingNode}
			setYAMLNode(node, "path", yamlScalar(in.Path))
			injects.Content = append(injects.Content, node)
		}
		setYAMLNode(entry, "inject", injects)
	}
	if c := strings.TrimSpace(p.Attach.Container); c != "" {
		attach := &yamlv3.Node{Kind: yamlv3.MappingNode}
		setYAMLNode(attach, "container", yamlScalar(c))
		setYAMLNode(entry, "attach", attach)
	}
	workloads.Content = append(workloads.Content, entry)

	// yaml.v3 defaults to 4-space indent and this rewrites the whole document,
	// so without matching the 2 spaces okdev init writes, adding a workload
	// reindents every untouched line of the user's config.
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(&doc); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	return buf.Bytes(), nil
}

// projectLevelInitFlags configure a project at creation time. They are
// meaningless when appending a workload, and are rejected rather than ignored
// so one flag never means two things.
var projectLevelInitFlags = []string{
	"name", "namespace", "context", "set",
	"dev-image", "sidecar-image", "sync-local", "sync-remote",
	"ssh-user", "shell", "stignore-preset",
}

func changedProjectFlags(cmd *cobra.Command) []string {
	var changed []string
	for _, name := range projectLevelInitFlags {
		if f := cmd.Flags().Lookup(name); f != nil && f.Changed {
			changed = append(changed, name)
		}
	}
	return changed
}

// existingConfigPath returns the config this project already has, or "" when
// there is none. It honours --config and OKDEV_CONFIG first and otherwise uses
// the same parent-directory discovery every other command uses, so an older
// flat .okdev.yaml is found rather than shadowed by a new folder config.
//
// Discovery finding nothing is a fresh init, and so is a --config naming a file
// that does not exist yet — that flag says *where to put* the new config.
//
// OKDEV_CONFIG is different: it claims the project's config already lives at a
// path. When that path is not there, every other okdev command fails with
// "config not found", and init must say the same. Swallowing it made init
// report "this is a new config" and blame whichever flag was checked next,
// while `okdev validate` in the same shell correctly named the missing file.
func existingConfigPath(opts *Options) (string, error) {
	p, err := config.ResolvePath(opts.ConfigPath)
	if err == nil {
		return p, nil
	}
	if strings.TrimSpace(opts.ConfigPath) == "" && strings.TrimSpace(os.Getenv(config.EnvConfigPath)) != "" {
		return "", err
	}
	return "", nil
}

// runInitAddWorkload appends a workload to an existing config. It writes the
// manifest and the config together or not at all.
func runInitAddWorkload(cmd *cobra.Command, cfgPath string, vars *config.TemplateVars, workloadName, templateRef string) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	add, err := planWorkloadAddition(cfgPath, raw, cfg, vars, workloadName, templateRef, config.RootDir(cfgPath))
	if err != nil {
		return err
	}

	if add.ManifestTarget != "" {
		if _, err := os.Stat(add.ManifestTarget); err == nil {
			// --force means "rewrite the config"; overloading it to also
			// clobber a manifest is how a flag becomes dangerous.
			return fmt.Errorf("a manifest already exists at %q; remove it or choose another --workload-name", add.ManifestTarget)
		}
		if err := os.MkdirAll(filepath.Dir(add.ManifestTarget), 0o755); err != nil {
			return fmt.Errorf("create manifest directory: %w", err)
		}
		if err := os.WriteFile(add.ManifestTarget, add.ManifestBytes, 0o644); err != nil {
			return fmt.Errorf("write manifest %q: %w", add.ManifestTarget, err)
		}
	}
	if err := os.WriteFile(cfgPath, add.ConfigBytes, 0o644); err != nil {
		return fmt.Errorf("write config %q: %w", cfgPath, err)
	}

	name := strings.TrimSpace(workloadName)
	out := cmd.OutOrStdout()
	if add.ManifestTarget != "" {
		fmt.Fprintf(out, "Wrote %s\n", add.ManifestTarget)
	}
	fmt.Fprintf(out, "Declared workload %q in %s\n", name, cfgPath)
	fmt.Fprintf(out, "next: okdev workload use %s && okdev up\n", name)
	return nil
}
