package cli

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	yamlv3 "gopkg.in/yaml.v3"
)

// podTemplateExtraction is the complete, not-yet-written result of moving an
// inline spec.podTemplate into its own manifest.
type podTemplateExtraction struct {
	ConfigBytes    []byte
	ManifestPath   string // as recorded in the config
	ManifestTarget string // absolute path to write
	ManifestBytes  []byte // empty when no workload needed one
	Warnings       []string
	Applied        bool
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
	if spec == nil || findYAMLNode(spec, "podTemplate") == nil {
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
	if target == nil {
		out.Warnings = append(out.Warnings,
			"spec.podTemplate was not used by any workload and has been dropped")
	} else {
		out.ManifestPath = extractedManifestPath(cfgPath)
		out.ManifestTarget = workload.ResolveManifestPath(cfgPath, out.ManifestPath)
		manifest, err := config.SynthesizePodManifest(cfg, "{{ .WorkloadName }}")
		if err != nil {
			return nil, err
		}
		out.ManifestBytes = manifest
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
