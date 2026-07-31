package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
	yamlv3 "gopkg.in/yaml.v3"
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
		// --template and --workload are one word apart, and their values look
		// alike from the outside: "pytorchjob" is both a workload type and a
		// plausible template name. Someone reaching for --template while naming
		// a workload almost certainly wants --workload, so say that instead of
		// the generic advice, which answers a question they did not ask.
		for _, flag := range inv.ProjectFlagsSet {
			if flag == "template" {
				return fmt.Errorf("--template selects the template that renders a whole config and cannot add a workload to an existing one; use --workload <pod|job|pytorchjob|generic> to choose the workload type")
			}
		}
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
func planWorkloadAddition(cfgPath string, raw []byte, cfg *config.DevEnvironment, vars *config.TemplateVars, workloadName string) (*workloadAddition, error) {
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

	// The location is this path's business; the inject paths and attach
	// container remain init's. Setting the path first keeps applyWorkloadDefaults
	// from substituting its own `.okdev/<type>.yaml`, which two workloads of the
	// same type would collide on.
	manifestPath := strings.TrimSpace(vars.ManifestPath)
	if manifestPath == "" && strings.TrimSpace(vars.WorkloadType) != workload.TypeGeneric {
		manifestPath = additiveManifestPath(cfgPath, name)
	}
	vars.ManifestPath = manifestPath
	applyWorkloadDefaults(vars)
	// applyWorkloadDefaults clears ManifestPath for pod, because init's
	// fresh-config path renders spec.podTemplate instead of a file. An added pod
	// always needs its own file: at most one workload may rely on the shared
	// podTemplate, and the config it is being added to already has that one.
	if manifestPath != "" {
		vars.ManifestPath = manifestPath
	}
	if err := validateInitWorkloadVars(vars); err != nil {
		return nil, err
	}

	manifestBytes, err := planWorkloadManifest(cfg, vars)
	if err != nil {
		return nil, err
	}

	profile := config.WorkloadProfile{
		Name:         name,
		Type:         vars.WorkloadType,
		ManifestPath: vars.ManifestPath,
	}
	for _, p := range vars.InjectPaths {
		if p = strings.TrimSpace(p); p != "" {
			profile.Inject = append(profile.Inject, config.WorkloadInjectSpec{Path: p})
		}
	}
	if c := strings.TrimSpace(vars.AttachContainer); c != "" {
		profile.Attach = config.WorkloadAttachSpec{Container: c}
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
		add.ManifestTarget = workload.ResolveManifestPath(cfgPath, vars.ManifestPath)
	}
	return add, nil
}

// planWorkloadManifest renders the manifest for a newly declared workload, or
// returns nil when the user is supplying their own file.
func planWorkloadManifest(cfg *config.DevEnvironment, vars *config.TemplateVars) ([]byte, error) {
	var asset string
	switch strings.TrimSpace(vars.WorkloadType) {
	case workload.TypePod:
		// A new pod workload starts as a copy of what the project already runs,
		// so the common case — the same container with different resources —
		// is an edit rather than a rewrite. The name stays a placeholder so
		// each run still gets a fresh object name.
		//
		// A project with no spec.podTemplate (a pytorchjob-only config, say) has
		// nothing to copy: synthesizing from it yields `containers: null`, which
		// the config validator accepts and the apiserver rejects at apply time.
		// Fall through to the starter template so the user gets a fillable
		// skeleton, the same as job and pytorchjob.
		if len(cfg.Spec.PodTemplate.Spec.Containers) > 0 {
			return config.SynthesizePodManifest(cfg, "{{ .WorkloadName }}")
		}
		asset = "templates/manifests/pod.yaml.tmpl"
	case workload.TypeJob:
		asset = "templates/manifests/job.yaml.tmpl"
	case workload.TypePyTorchJob:
		asset = "templates/manifests/pytorchjob.yaml.tmpl"
	case workload.TypeGeneric:
		if strings.TrimSpace(vars.GenericPreset) != "deployment" {
			return nil, nil // the user supplies the manifest
		}
		asset = "templates/manifests/deployment.yaml.tmpl"
	default:
		return nil, fmt.Errorf("unsupported workload type %q", vars.WorkloadType)
	}
	rendered, err := config.RenderEmbeddedTemplate(asset, vars)
	if err != nil {
		return nil, err
	}
	return []byte(rendered), nil
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
	workloads.Content = append(workloads.Content, entry)

	out, err := yamlv3.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("render config: %w", err)
	}
	return out, nil
}

// projectLevelInitFlags configure a project at creation time. They are
// meaningless when appending a workload, and are rejected rather than ignored
// so one flag never means two things.
var projectLevelInitFlags = []string{
	"name", "namespace", "context", "template", "set",
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
func runInitAddWorkload(cmd *cobra.Command, cfgPath string, vars *config.TemplateVars, workloadName string) error {
	raw, err := os.ReadFile(cfgPath)
	if err != nil {
		return fmt.Errorf("read config %q: %w", cfgPath, err)
	}
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	add, err := planWorkloadAddition(cfgPath, raw, cfg, vars, workloadName)
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
