package config

import (
	"fmt"

	"gopkg.in/yaml.v3"
)

// Migration defines a single config transformation.
type Migration struct {
	Name        string
	Description string
	Applies     func(doc *yaml.Node) bool
	Transform   func(doc *yaml.Node) (warnings []string, err error)
}

// MigrationResult holds the outcome of running migrations.
type MigrationResult struct {
	Applied  []string // names of applied migrations
	Warnings []string // warnings from ambiguous transforms
}

// RunMigrations runs all applicable migrations sequentially.
// Each migration checks Applies() before running Transform().
func RunMigrations(doc *yaml.Node, migrations []Migration) (*MigrationResult, error) {
	result := &MigrationResult{}
	for _, m := range migrations {
		if !m.Applies(doc) {
			continue
		}
		warnings, err := m.Transform(doc)
		if err != nil {
			return nil, fmt.Errorf("migration %q: %w", m.Name, err)
		}
		result.Applied = append(result.Applied, m.Name)
		result.Warnings = append(result.Warnings, warnings...)
	}
	return result, nil
}

// findSpecNode navigates doc → spec mapping node.
// Returns nil if not found.
func findSpecNode(doc *yaml.Node) *yaml.Node {
	root := doc
	if root.Kind == yaml.DocumentNode && len(root.Content) > 0 {
		root = root.Content[0]
	}
	if root.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i < len(root.Content)-1; i += 2 {
		if root.Content[i].Value == "spec" && root.Content[i+1].Kind == yaml.MappingNode {
			return root.Content[i+1]
		}
	}
	return nil
}

// findKey finds a key in a mapping node and returns (keyNode, valueNode).
// Returns (nil, nil) if not found.
func findKey(mapping *yaml.Node, key string) (*yaml.Node, *yaml.Node) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, nil
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			return mapping.Content[i], mapping.Content[i+1]
		}
	}
	return nil, nil
}

// removeKey removes a key-value pair from a mapping node.
func removeKey(mapping *yaml.Node, key string) {
	if mapping == nil || mapping.Kind != yaml.MappingNode {
		return
	}
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content = append(mapping.Content[:i], mapping.Content[i+2:]...)
			return
		}
	}
}

// setKey sets or adds a key-value pair in a mapping node.
func setKey(mapping *yaml.Node, key string, value *yaml.Node) {
	for i := 0; i < len(mapping.Content)-1; i += 2 {
		if mapping.Content[i].Value == key {
			mapping.Content[i+1] = value
			return
		}
	}
	mapping.Content = append(mapping.Content,
		&yaml.Node{Kind: yaml.ScalarNode, Value: key, Tag: "!!str"},
		value,
	)
}

// DefaultMigrations is the ordered list of all config migrations.
var DefaultMigrations = []Migration{
	workspaceToVolumesMigration(),
}

// knownPVCKeys are the workspace.pvc keys we know how to migrate.
var knownPVCKeys = map[string]bool{
	"claimName":        true,
	"size":             true,
	"storageClassName": true,
}

func workspaceToVolumesMigration() Migration {
	return Migration{
		Name:        "workspace-to-volumes",
		Description: "Migrate spec.workspace to spec.volumes + podTemplate volumeMounts",
		Applies: func(doc *yaml.Node) bool {
			spec := findSpecNode(doc)
			_, val := findKey(spec, "workspace")
			return val != nil
		},
		Transform: func(doc *yaml.Node) ([]string, error) {
			spec := findSpecNode(doc)
			if spec == nil {
				return nil, fmt.Errorf("spec node not found")
			}
			_, wsNode := findKey(spec, "workspace")
			if wsNode == nil || wsNode.Kind != yaml.MappingNode {
				return nil, fmt.Errorf("workspace node is not a mapping")
			}

			var warnings []string
			mountPath := "/workspace" // default
			claimName := ""
			size := ""
			storageClassName := ""

			// Extract workspace fields
			if _, v := findKey(wsNode, "mountPath"); v != nil {
				mountPath = v.Value
			}
			if _, pvcNode := findKey(wsNode, "pvc"); pvcNode != nil && pvcNode.Kind == yaml.MappingNode {
				for i := 0; i < len(pvcNode.Content)-1; i += 2 {
					key := pvcNode.Content[i].Value
					val := pvcNode.Content[i+1].Value
					switch key {
					case "claimName":
						claimName = val
					case "size":
						size = val
					case "storageClassName":
						storageClassName = val
					default:
						if !knownPVCKeys[key] {
							warnings = append(warnings,
								fmt.Sprintf("unknown workspace.pvc key %q = %q -- review manually", key, val))
						}
					}
				}
			}

			// Build volumes node
			volumeMapping := &yaml.Node{Kind: yaml.MappingNode}
			setKey(volumeMapping, "name", &yaml.Node{Kind: yaml.ScalarNode, Value: "workspace", Tag: "!!str"})

			pvcMapping := &yaml.Node{Kind: yaml.MappingNode}
			if claimName != "" {
				setKey(pvcMapping, "claimName", &yaml.Node{Kind: yaml.ScalarNode, Value: claimName, Tag: "!!str"})
			}

			setKey(volumeMapping, "persistentVolumeClaim", pvcMapping)

			volumesSeq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{volumeMapping}}

			// Build podTemplate node
			vmMapping := &yaml.Node{Kind: yaml.MappingNode}
			setKey(vmMapping, "name", &yaml.Node{Kind: yaml.ScalarNode, Value: "workspace", Tag: "!!str"})
			setKey(vmMapping, "mountPath", &yaml.Node{Kind: yaml.ScalarNode, Value: mountPath, Tag: "!!str"})
			vmSeq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{vmMapping}}

			containerMapping := &yaml.Node{Kind: yaml.MappingNode}
			setKey(containerMapping, "name", &yaml.Node{Kind: yaml.ScalarNode, Value: "dev", Tag: "!!str"})
			setKey(containerMapping, "volumeMounts", vmSeq)
			containerSeq := &yaml.Node{Kind: yaml.SequenceNode, Content: []*yaml.Node{containerMapping}}

			podSpec := &yaml.Node{Kind: yaml.MappingNode}
			setKey(podSpec, "containers", containerSeq)
			podTemplate := &yaml.Node{Kind: yaml.MappingNode}
			setKey(podTemplate, "spec", podSpec)

			// Remove workspace, add volumes and podTemplate
			removeKey(spec, "workspace")

			// Merge with existing volumes if present
			_, existingVolumes := findKey(spec, "volumes")
			if existingVolumes != nil && existingVolumes.Kind == yaml.SequenceNode {
				existingVolumes.Content = append(existingVolumes.Content, volumeMapping)
			} else {
				setKey(spec, "volumes", volumesSeq)
			}

			// Merge with existing podTemplate if present
			_, existingPT := findKey(spec, "podTemplate")
			if existingPT != nil && existingPT.Kind == yaml.MappingNode {
				_, existingPTSpec := findKey(existingPT, "spec")
				if existingPTSpec != nil && existingPTSpec.Kind == yaml.MappingNode {
					_, existingContainers := findKey(existingPTSpec, "containers")
					if existingContainers != nil && existingContainers.Kind == yaml.SequenceNode {
						// Find the "dev" container and add volumeMounts
						for _, c := range existingContainers.Content {
							if c.Kind == yaml.MappingNode {
								_, nameNode := findKey(c, "name")
								if nameNode != nil && nameNode.Value == "dev" {
									setKey(c, "volumeMounts", vmSeq)
									goto podTemplateDone
								}
							}
						}
						// No "dev" container found, add one
						existingContainers.Content = append(existingContainers.Content, containerMapping)
					} else {
						setKey(existingPTSpec, "containers", containerSeq)
					}
				} else {
					setKey(existingPT, "spec", podSpec)
				}
			} else {
				setKey(spec, "podTemplate", podTemplate)
			}
		podTemplateDone:

			// Add YAML comments and warnings for fields that need manual review
			if size != "" {
				volumeMapping.HeadComment = fmt.Sprintf("TODO: PVC size was %q -- set resources.requests.storage on the PVC object if needed", size)
				warnings = append(warnings,
					fmt.Sprintf("Review PVC size %q -- set resources.requests.storage on the PVC object if needed", size))
			}
			if storageClassName != "" {
				if volumeMapping.HeadComment != "" {
					volumeMapping.HeadComment += "\n"
				}
				volumeMapping.HeadComment += fmt.Sprintf("TODO: storageClassName was %q -- set on the PVC object if needed", storageClassName)
				warnings = append(warnings,
					fmt.Sprintf("Review storageClassName %q -- set on the PVC object if needed", storageClassName))
			}

			return warnings, nil
		},
	}
}
