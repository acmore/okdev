package cli

import (
	"bytes"
	"fmt"
	"os"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	yamlv3 "gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"
)

// volumeMove is the not-yet-written result of moving spec.volumes into the
// manifests of every workload that should carry them.
type volumeMove struct {
	ConfigBytes []byte
	Manifests   []plannedManifest
	Warnings    []string
	Applied     bool
}

// planVolumeMove moves spec.volumes into each workload's manifest.
//
// spec.volumes merged volumes into a Kubernetes object the user wrote, and lost
// every name conflict to it silently — a workspace PVC in the config became an
// emptyDir the moment the manifest declared one. Volumes belong to the manifest,
// so they move there.
//
// pending are manifests another step of the same migration is about to write;
// those are edited in memory rather than read from disk, so the two compose.
func planVolumeMove(cfgPath string, raw []byte, pending []plannedManifest) (*volumeMove, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return nil, fmt.Errorf("parse config: %w", err)
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return nil, fmt.Errorf("config is not a mapping")
	}
	spec := findYAMLNode(root, "spec")
	if spec == nil || findYAMLNode(spec, "volumes") == nil {
		return &volumeMove{}, nil
	}

	cfg, err := config.LoadPodTemplateOnly(raw)
	if err != nil {
		return nil, err
	}
	if len(cfg.Spec.Volumes) == 0 {
		removeYAMLKey(spec, "volumes")
		rendered, err := renderYAMLDoc(&doc)
		if err != nil {
			return nil, err
		}
		return &volumeMove{ConfigBytes: rendered, Applied: true}, nil
	}

	// A comment on spec.volumes describes those volumes, so it travels with
	// them. Losing it would leave the user's own note only in the .bak they
	// will never open again.
	comment := yamlKeyComment(spec, "volumes")

	out := &volumeMove{Applied: true}
	byPath := map[string]*plannedManifest{}
	for i := range pending {
		byPath[pending[i].Path] = &pending[i]
	}

	// Every profile's manifest gets a copy: spec.volumes was shared, and a
	// Kubernetes object declares what it needs.
	for _, path := range configManifestPaths(cfg) {
		target := workload.ResolveManifestPath(cfgPath, path)
		var body []byte
		fromPending := false
		if m, ok := byPath[path]; ok {
			body, fromPending = m.Bytes, true
		} else {
			read, err := os.ReadFile(target)
			if err != nil {
				// A workload whose manifest is missing cannot run anyway, and
				// refusing the whole migration over one stale path would strand
				// every other workload. Say so and move on.
				out.Warnings = append(out.Warnings, fmt.Sprintf(
					"workload manifest %s does not exist; its copy of spec.volumes was dropped — declare them there once the file exists", path))
				continue
			}
			body = read
		}

		merged, skipped, err := addVolumesToManifest(body, cfg.Spec.Volumes, comment)
		if err != nil {
			return nil, fmt.Errorf("add volumes to %q: %w", target, err)
		}
		for _, name := range skipped {
			out.Warnings = append(out.Warnings, fmt.Sprintf(
				"%s already declares a volume named %q; the config's copy was dropped, and the manifest's was already the one in effect",
				path, name))
		}
		out.Manifests = append(out.Manifests, plannedManifest{
			Path:   path,
			Target: target,
			Bytes:  merged,
			// Only a file okdev did not just author needs preserving.
			Backup: !fromPending,
		})
	}

	removeYAMLKey(spec, "volumes")
	rendered, err := renderYAMLDoc(&doc)
	if err != nil {
		return nil, err
	}
	out.ConfigBytes = rendered
	return out, nil
}

// configManifestPaths lists each declared workload's manifest, deduplicated:
// two profiles may legitimately share one file.
func configManifestPaths(cfg *config.DevEnvironment) []string {
	seen := map[string]struct{}{}
	var paths []string
	add := func(p string) {
		p = strings.TrimSpace(p)
		if p == "" {
			return
		}
		if _, dup := seen[p]; dup {
			return
		}
		seen[p] = struct{}{}
		paths = append(paths, p)
	}
	for _, p := range cfg.Spec.Workloads {
		add(p.ManifestPath)
	}
	add(cfg.Spec.Workload.ManifestPath)
	return paths
}

// addVolumesToManifest appends volumes the manifest does not already declare,
// and reports the names it skipped. A name the manifest declares is left alone:
// that definition was already the one in effect.
func addVolumesToManifest(manifest []byte, volumes []corev1.Volume, comment string) ([]byte, []string, error) {
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(manifest, &doc); err != nil {
		return nil, nil, err
	}
	root := yamlDocumentRoot(&doc)
	if root == nil || root.Kind != yamlv3.MappingNode {
		return nil, nil, fmt.Errorf("manifest is not a mapping")
	}
	// Volumes live beside the containers, which for a Pod is spec and for a
	// controller is the pod template's spec.
	specs := podSpecNodes(root)
	if len(specs) == 0 {
		return nil, nil, fmt.Errorf("no pod spec found to hold the volumes")
	}

	var skipped []string
	for _, podSpec := range specs {
		list := findYAMLNode(podSpec, "volumes")
		if list == nil {
			list = &yamlv3.Node{Kind: yamlv3.SequenceNode}
			setYAMLNode(podSpec, "volumes", list)
			// Only on a key this migration created; one the manifest already
			// had may carry a comment of its own.
			setYAMLKeyComment(podSpec, "volumes", comment)
		}
		declared := map[string]struct{}{}
		for _, item := range list.Content {
			if n := findYAMLNode(item, "name"); n != nil {
				declared[strings.TrimSpace(n.Value)] = struct{}{}
			}
		}
		for _, v := range volumes {
			if _, dup := declared[v.Name]; dup {
				skipped = append(skipped, v.Name)
				continue
			}
			node, err := volumeNode(v)
			if err != nil {
				return nil, nil, err
			}
			list.Content = append(list.Content, node)
		}
	}
	rendered, err := renderYAMLDoc(&doc)
	if err != nil {
		return nil, nil, err
	}
	return rendered, dedupeStrings(skipped), nil
}

// volumeNode round-trips a volume through YAML so the manifest carries exactly
// what the config declared, whatever volume source it used.
func volumeNode(v corev1.Volume) (*yamlv3.Node, error) {
	raw, err := sigsYAMLMarshal(v)
	if err != nil {
		return nil, err
	}
	var doc yamlv3.Node
	if err := yamlv3.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	node := yamlDocumentRoot(&doc)
	if node == nil {
		return nil, fmt.Errorf("volume %q rendered nothing", v.Name)
	}
	return node, nil
}

func dedupeStrings(in []string) []string {
	seen := map[string]struct{}{}
	var out []string
	for _, s := range in {
		if _, dup := seen[s]; dup {
			continue
		}
		seen[s] = struct{}{}
		out = append(out, s)
	}
	return out
}

// podSpecNodes finds the pod specs in a manifest: the object's own spec for a
// Pod, or every pod template's spec for a controller. Volumes go beside the
// containers they serve.
func podSpecNodes(root *yamlv3.Node) []*yamlv3.Node {
	spec := findYAMLNode(root, "spec")
	if spec == nil {
		return nil
	}
	if findYAMLNode(spec, "containers") != nil {
		return []*yamlv3.Node{spec}
	}
	var out []*yamlv3.Node
	var walk func(n *yamlv3.Node)
	walk = func(n *yamlv3.Node) {
		if n == nil || n.Kind != yamlv3.MappingNode {
			return
		}
		if tmpl := findYAMLNode(n, "template"); tmpl != nil {
			if s := findYAMLNode(tmpl, "spec"); s != nil && findYAMLNode(s, "containers") != nil {
				out = append(out, s)
			}
		}
		for i := 1; i < len(n.Content); i += 2 {
			walk(n.Content[i])
		}
	}
	walk(spec)
	return out
}

// renderYAMLDoc writes a node tree back out at the 2-space indent okdev's own
// files use, so an edit does not reindent every untouched line.
func renderYAMLDoc(doc *yamlv3.Node) ([]byte, error) {
	var buf bytes.Buffer
	enc := yamlv3.NewEncoder(&buf)
	enc.SetIndent(2)
	if err := enc.Encode(doc); err != nil {
		return nil, fmt.Errorf("render yaml: %w", err)
	}
	if err := enc.Close(); err != nil {
		return nil, fmt.Errorf("render yaml: %w", err)
	}
	return buf.Bytes(), nil
}

func sigsYAMLMarshal(v any) ([]byte, error) { return sigsyaml.Marshal(v) }

// yamlKeyComment returns the comment written above a mapping key, which yaml.v3
// attaches to the key node rather than its value.
func yamlKeyComment(mapping *yamlv3.Node, key string) string {
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i].HeadComment
		}
	}
	return ""
}

func setYAMLKeyComment(mapping *yamlv3.Node, key, comment string) {
	if strings.TrimSpace(comment) == "" {
		return
	}
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i].HeadComment = comment
			return
		}
	}
}
