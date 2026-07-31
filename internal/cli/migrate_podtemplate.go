package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	yamlv3 "gopkg.in/yaml.v3"
)

// podTemplateExtraction is the complete, not-yet-written result of giving a
// pod workload a manifest of its own.
type podTemplateExtraction struct {
	ConfigBytes    []byte
	ManifestPath   string // as recorded in the config
	ManifestTarget string // absolute path to write
	ManifestBytes  []byte // empty when no workload needed one
	Warnings       []string
	Applied        bool
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
	target := podTemplateTargetProfile(spec)
	switch {
	case target == nil:
		out.Warnings = append(out.Warnings,
			"spec.podTemplate was not used by any workload and has been dropped")
	default:
		out.ManifestPath = extractedManifestPath(cfgPath)
		out.ManifestTarget = workload.ResolveManifestPath(cfgPath, out.ManifestPath)
		manifest, err := podManifestForMigration(cfg)
		if err != nil {
			return nil, err
		}
		out.ManifestBytes = manifest
		out.Extracted = hasPodTemplate && len(cfg.Spec.PodTemplate.Spec.Containers) > 0
		if !out.Extracted {
			out.Warnings = append(out.Warnings,
				"this pod workload declared no container of its own; wrote the starter manifest okdev used to apply by default")
		}
		setYAMLNode(target, "manifestPath", yamlScalar(out.ManifestPath))
		if findYAMLNode(target, "type") == nil {
			setYAMLNode(target, "type", yamlScalar(workload.TypePod))
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
	if cfg.Spec.PodTemplate != nil && len(cfg.Spec.PodTemplate.Spec.Containers) > 0 {
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
func extractedManifestPath(cfgPath string) string {
	if isFolderConfigPath(cfgPath) {
		return "pod.yaml"
	}
	return filepath.Join(".okdev", "pod.yaml")
}

// podTemplateTargetProfile finds the workload that was using spec.podTemplate:
// the pod with no manifestPath. Validation guarantees at most one. It
// materializes spec.workloads with a "default" entry when the config declares
// none, and returns nil when every profile already has its own manifest.
func podTemplateTargetProfile(spec *yamlv3.Node) *yamlv3.Node {
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
		return entry
	}
	for _, entry := range workloads.Content {
		t := findYAMLNode(entry, "type")
		if t != nil && !isPodTypeValue(t.Value) {
			continue
		}
		if findYAMLNode(entry, "manifestPath") == nil {
			return entry
		}
	}
	return nil
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
