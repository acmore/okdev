package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	yamlv3 "gopkg.in/yaml.v3"
)

// plannedManifest is one manifest file the migration will write.
type plannedManifest struct {
	Path   string // as recorded in the config
	Target string // absolute path to write
	Bytes  []byte
	// Backup marks a file okdev did not author in this run, so the migration
	// preserves what was there before overwriting it.
	Backup bool
}

// podTemplateExtraction is the complete, not-yet-written result of giving every
// pod workload a manifest of its own.
type podTemplateExtraction struct {
	ConfigBytes []byte
	Manifests   []plannedManifest
	Warnings    []string
	Applied     bool
	// Extracted distinguishes the two cases for reporting: an inline
	// spec.podTemplate moved into a file, or a pod that never had one getting
	// the starter manifest.
	Extracted bool
}

// Label names the migration in `okdev migrate` output.
func (e *podTemplateExtraction) Label() string {
	if e.Extracted {
		return "podTemplate-to-manifest"
	}
	return "pod-workload-manifest"
}

// planPodTemplateExtraction computes the migration without touching the
// filesystem, so a refusal can leave the project byte-identical.
func planPodTemplateExtraction(cfgPath string, raw []byte) (*podTemplateExtraction, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	spec := findYAMLNode(root, "spec")
	if spec == nil {
		return &podTemplateExtraction{}, nil
	}
	// Two shapes need a manifest: an inline spec.podTemplate to extract, and a
	// pod workload that never had one. The second used to run on a hardcoded
	// default container, which was just another inline form.
	hasPodTemplate := findYAMLNode(spec, "podTemplate") != nil
	if !hasPodTemplate && !hasManifestlessPodWorkload(spec) {
		return &podTemplateExtraction{}, nil
	}

	// Decode through the config struct so the manifest is built from the same
	// representation okdev applied, rather than a second YAML interpretation.
	cfg, err := config.LoadPodTemplateOnly(raw)
	if err != nil {
		return nil, err
	}

	out := &podTemplateExtraction{Applied: true}
	targets := podTemplateTargetProfiles(spec)
	switch {
	case len(targets) == 0:
		out.Warnings = append(out.Warnings,
			"spec.podTemplate was not used by any workload and has been dropped")
	default:
		manifest, err := podManifestForMigration(cfg)
		if err != nil {
			return nil, err
		}
		// hasPodTemplate only says the YAML key is present. `podTemplate:` with
		// nothing under it decodes to a nil pointer, so ask the decoded config
		// rather than the key — dereferencing on the key alone panicked.
		out.Extracted = podTemplateHasContainers(cfg)
		if !out.Extracted {
			out.Warnings = append(out.Warnings,
				"this pod workload declared no container of its own; wrote the starter manifest okdev used to apply by default")
		}
		// Every manifest-less pod profile was synthesized from the same shared
		// podTemplate, so they all get the same starting content and a file
		// named after the profile. Migrating only the first would report
		// success and leave the config invalid.
		for _, target := range targets {
			path := extractedManifestPath(cfgPath, profileManifestBasename(target, len(targets)))
			out.Manifests = append(out.Manifests, plannedManifest{
				Path:   path,
				Target: workload.ResolveManifestPath(cfgPath, path),
				Bytes:  manifest,
			})
			setYAMLNode(target, "manifestPath", yamlScalar(path))
			if findYAMLNode(target, "type") == nil {
				setYAMLNode(target, "type", yamlScalar(workload.TypePod))
			}
		}
		if len(targets) > 1 {
			out.Warnings = append(out.Warnings,
				"several pod workloads shared one spec.podTemplate; each now has its own manifest with identical content — edit them to differ")
		}
	}

	removeYAMLKey(spec, "podTemplate")

	rendered, err := yamlv3.Marshal(&doc)
	if err != nil {
		return nil, fmt.Errorf("render migrated config: %w", err)
	}
	out.ConfigBytes = rendered
	return out, nil
}

// hasManifestlessPodWorkload reports whether the config runs a pod workload
// with no manifest of its own — including the config that declares no workload
// at all, which defaults to pod.
func hasManifestlessPodWorkload(spec *yamlv3.Node) bool {
	if workloads := findYAMLNode(spec, "workloads"); workloads != nil {
		for _, entry := range workloads.Content {
			t := findYAMLNode(entry, "type")
			if t != nil && !isPodTypeValue(t.Value) {
				continue
			}
			if findYAMLNode(entry, "manifestPath") == nil {
				return true
			}
		}
		return false
	}
	legacy := findYAMLNode(spec, "workload")
	if legacy == nil {
		return true // no workload declared at all: pod by default
	}
	if t := findYAMLNode(legacy, "type"); t != nil && !isPodTypeValue(t.Value) {
		return false
	}
	return findYAMLNode(legacy, "manifestPath") == nil
}

// podManifestForMigration renders the manifest a pod workload should carry:
// the extracted spec.podTemplate when there is one, otherwise the starter
// manifest — which reproduces the container okdev used to inject when a pod
// spec declared none.
func podManifestForMigration(cfg *config.DevEnvironment) ([]byte, error) {
	if podTemplateHasContainers(cfg) {
		return synthesizePodManifestTemplate(cfg)
	}
	vars := config.NewTemplateVars()
	if remote := primarySyncRemote(cfg); remote != "" {
		vars.SyncRemote = remote
	}
	rendered, err := config.RenderEmbeddedTemplate("templates/manifests/pod.yaml.tmpl", vars)
	if err != nil {
		return nil, err
	}
	return []byte(rendered), nil
}

// primarySyncRemote is the remote root of the first sync mapping, so the
// scaffolded manifest mounts the workspace where the config already syncs it.
func primarySyncRemote(cfg *config.DevEnvironment) string {
	for _, p := range cfg.Spec.Sync.Paths {
		if remote := strings.TrimSpace(p.Remote); remote != "" {
			return remote
		}
	}
	return ""
}

// workloadNameSentinel is a name no pod spec would contain, swapped for the
// runtime placeholder only after the user's own braces have been escaped.
const workloadNameSentinel = "okdev-workload-name-sentinel"

// synthesizePodManifestTemplate renders spec.podTemplate as a Pod manifest that
// is safe to feed the workload template renderer.
//
// A synthesized pod manifest used to be applied verbatim, so a pod spec could
// legitimately contain "{{" — an arg for some other templating tool, say. File
// manifests are rendered as Go templates, so those braces have to be escaped or
// a config that worked before migration fails to render after it. Only the name
// okdev substitutes is left as a live placeholder.
func synthesizePodManifestTemplate(cfg *config.DevEnvironment) ([]byte, error) {
	raw, err := config.SynthesizePodManifest(cfg, workloadNameSentinel)
	if err != nil {
		return nil, err
	}
	escaped := strings.ReplaceAll(string(raw), "{{", "{{`{{`}}")
	// Quoted, because an unquoted `{{ ... }}` is a YAML flow mapping. The
	// sentinel was marshaled as a plain scalar, so the quotes go on here.
	return []byte(strings.ReplaceAll(escaped, workloadNameSentinel, "'{{ .WorkloadName }}'")), nil
}

// extractedManifestPath keeps the manifest inside .okdev/ for either config
// shape: a folder config's own directory already is .okdev/, a flat config's
// is the project root.
func extractedManifestPath(cfgPath, basename string) string {
	if isFolderConfigPath(cfgPath) {
		return basename
	}
	return filepath.Join(".okdev", basename)
}

// profileManifestBasename names the file after its workload when several need
// one, and "pod.yaml" for the ordinary single-workload config.
func profileManifestBasename(profile *yamlv3.Node, total int) string {
	if total < 2 {
		return "pod.yaml"
	}
	if name := findYAMLNode(profile, "name"); name != nil && strings.TrimSpace(name.Value) != "" {
		return strings.TrimSpace(name.Value) + ".yaml"
	}
	return "pod.yaml"
}

// podTemplateTargetProfiles finds the workloads that were using
// spec.podTemplate: the pods with no manifestPath. It materializes
// spec.workloads with a "default" entry when the config declares none, and
// returns nothing when every profile already has its own manifest.
func podTemplateTargetProfiles(spec *yamlv3.Node) []*yamlv3.Node {
	workloads := findYAMLNode(spec, "workloads")
	if workloads == nil {
		if legacy := findYAMLNode(spec, "workload"); legacy != nil {
			if t := findYAMLNode(legacy, "type"); t != nil && !isPodTypeValue(t.Value) {
				return nil
			}
			if findYAMLNode(legacy, "manifestPath") != nil {
				return nil
			}
		}
		entry := &yamlv3.Node{Kind: yamlv3.MappingNode}
		setYAMLNode(entry, "name", yamlScalar(config.DefaultWorkloadProfileName))
		// Carry the legacy singular block's settings across before dropping it,
		// or an attach.container the user set is silently lost.
		if legacy := findYAMLNode(spec, "workload"); legacy != nil {
			for _, key := range []string{"type", "inject", "attach"} {
				if v := findYAMLNode(legacy, key); v != nil {
					setYAMLNode(entry, key, v)
				}
			}
		}
		if findYAMLNode(entry, "type") == nil {
			setYAMLNode(entry, "type", yamlScalar(workload.TypePod))
		}
		seq := &yamlv3.Node{Kind: yamlv3.SequenceNode, Content: []*yamlv3.Node{entry}}
		setYAMLNode(spec, "workloads", seq)
		removeYAMLKey(spec, "workload")
		return []*yamlv3.Node{entry}
	}
	var targets []*yamlv3.Node
	for _, entry := range workloads.Content {
		t := findYAMLNode(entry, "type")
		if t != nil && !isPodTypeValue(t.Value) {
			continue
		}
		if findYAMLNode(entry, "manifestPath") == nil {
			targets = append(targets, entry)
		}
	}
	return targets
}

func isPodTypeValue(v string) bool {
	v = strings.TrimSpace(v)
	return v == "" || v == workload.TypePod
}

// removeYAMLKey deletes a key and its value from a mapping node.
func removeYAMLKey(mapping *yamlv3.Node, key string) {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// podTemplateHasContainers reports whether there is an inline pod spec worth
// extracting. A `podTemplate:` key with no body, or one with no containers,
// has nothing to carry over and gets the starter manifest instead.
func podTemplateHasContainers(cfg *config.DevEnvironment) bool {
	return cfg != nil && cfg.Spec.PodTemplate != nil && len(cfg.Spec.PodTemplate.Spec.Containers) > 0
}
