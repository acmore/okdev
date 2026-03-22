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
