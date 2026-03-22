# Config Migration and Interactive Init Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Add `okdev migrate` command for auto-transforming deprecated configs, redesign the template system to support user-provided templates, and make `okdev init` interactive by default.

**Architecture:** Migration uses `gopkg.in/yaml.v3` node API to preserve comments/formatting while transforming deprecated fields. Templates are rewritten as `//go:embed` `.yaml.tmpl` files rendered with `text/template`. Interactive prompts use `github.com/charmbracelet/huh` (or `github.com/AlecAivazis/survey/v2`) in `internal/cli/prompt.go`, keeping `internal/config/` free of tty dependencies.

**Tech Stack:** Go, `gopkg.in/yaml.v3`, `text/template`, `//go:embed`, cobra, charmbracelet/huh (prompt library)

**Spec:** `docs/superpowers/specs/2026-03-21-config-migration-and-interactive-init-design.md`

---

## File Structure

```
internal/config/
  config.go          # MODIFY: update Validate() error message for spec.workspace
  migrate.go         # CREATE: Migration type, registry, RunMigrations(), YAML node helpers
  migrate_test.go    # CREATE: tests for migration registry and workspace-to-volumes
  template.go        # REWRITE: TemplateVars, ResolveTemplate(), RenderTemplate()
  template_test.go   # REWRITE: tests for template resolution and rendering
  templates/
    basic.yaml.tmpl      # CREATE: embedded basic template
    gpu.yaml.tmpl        # CREATE: embedded GPU template
    llm-stack.yaml.tmpl  # CREATE: embedded LLM stack template

internal/cli/
  root.go            # MODIFY: register migrate command
  init.go            # REWRITE: use new template + prompt system
  prompt.go          # CREATE: interactive prompt functions
  prompt_test.go     # CREATE: tests for prompt defaults and flag overrides
  migrate.go         # CREATE: okdev migrate cobra command
```

---

## Chunk 1: Migration System

### Task 1: Migration Registry and YAML Node Helpers

**Files:**
- Create: `internal/config/migrate.go`
- Create: `internal/config/migrate_test.go`

- [ ] **Step 1: Write failing test for Migration type and RunMigrations with no migrations**

In `internal/config/migrate_test.go`:

```go
package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunMigrationsNoOp(t *testing.T) {
	input := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: test\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected 0 applied migrations, got %d", len(result.Applied))
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d", len(result.Warnings))
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run TestRunMigrationsNoOp -v`
Expected: FAIL — `RunMigrations` undefined

- [ ] **Step 3: Implement Migration type, MigrationResult, RunMigrations, and YAML node helpers**

In `internal/config/migrate.go`:

```go
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
```

- [ ] **Step 4: Run test to verify it passes**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run TestRunMigrationsNoOp -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/migrate.go internal/config/migrate_test.go
git commit -m "feat(config): add migration registry and YAML node helpers"
```

---

### Task 2: workspace-to-volumes Migration

**Files:**
- Modify: `internal/config/migrate.go`
- Modify: `internal/config/migrate_test.go`

- [ ] **Step 1: Write failing test for workspace-to-volumes migration**

Append to `internal/config/migrate_test.go`. First, update the import block at the top of the file to include all needed imports:

```go
import (
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
	sigyaml "sigs.k8s.io/yaml"
)
```

Then add these test functions and helpers:

```go
func loadFromBytes(data []byte) (*DevEnvironment, error) {
	var cfg DevEnvironment
	if err := sigyaml.Unmarshal(data, &cfg); err != nil {
		return nil, err
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func TestWorkspaceToVolumesMigration(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /code
    pvc:
      claimName: my-pvc
      size: 100Gi
      storageClassName: fast-ssd
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 1 || result.Applied[0] != "workspace-to-volumes" {
		t.Fatalf("expected workspace-to-volumes applied, got %v", result.Applied)
	}

	// Verify the migrated YAML round-trips through config.Load
	out, err := yaml.Marshal(&doc)
	if err != nil {
		t.Fatal(err)
	}

	// Verify workspace key is removed
	if strings.Contains(string(out), "workspace:") {
		t.Fatal("expected workspace key to be removed from output")
	}

	// Verify volumes and podTemplate are present
	if !strings.Contains(string(out), "volumes:") {
		t.Fatal("expected volumes key in output")
	}
	if !strings.Contains(string(out), "podTemplate:") {
		t.Fatal("expected podTemplate key in output")
	}

	// Round-trip: load through sigs.k8s.io/yaml to verify compatibility
	cfg, err := loadFromBytes(out)
	if err != nil {
		t.Fatalf("migrated config failed to load: %v", err)
	}
	if len(cfg.Spec.Volumes) == 0 {
		t.Fatal("expected volumes after migration")
	}
	if cfg.WorkspaceMountPath() != "/code" {
		t.Fatalf("expected mount path /code, got %q", cfg.WorkspaceMountPath())
	}
}

func TestWorkspaceToVolumesIdempotent(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /code
    pvc:
      claimName: my-pvc
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	// Run twice
	if _, err := RunMigrations(&doc, DefaultMigrations); err != nil {
		t.Fatal(err)
	}
	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected 0 applied on second run, got %v", result.Applied)
	}
}

func TestWorkspaceToVolumesUnknownPVCKeys(t *testing.T) {
	input := `apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: default
  workspace:
    mountPath: /workspace
    pvc:
      claimName: my-pvc
      unknownKey: some-value
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, DefaultMigrations)
	if err != nil {
		t.Fatal(err)
	}
	// Should have a warning about unknown key
	found := false
	for _, w := range result.Warnings {
		if strings.Contains(w, "unknownKey") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected warning about unknownKey, got %v", result.Warnings)
	}
}

```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run TestWorkspaceTo -v`
Expected: FAIL — `DefaultMigrations` undefined

- [ ] **Step 3: Implement workspace-to-volumes migration**

Add to `internal/config/migrate.go`:

```go
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
						warnings = append(warnings,
							fmt.Sprintf("unknown workspace.pvc key %q = %q -- review manually", key, val))
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
				// Insert comment on the volumes node for visibility
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
```

Note: The `size` and `storageClassName` fields are deliberately not auto-migrated into the new PVC structure because the new `spec.volumes` uses native Kubernetes `corev1.Volume` which has a different schema (`persistentVolumeClaim` only has `claimName`, `readOnly`). The full PVC spec with `storageClassName` and `resources.requests.storage` would need a separate `PersistentVolumeClaim` object. These are left as warnings for the user.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run "TestWorkspaceTo|TestRunMigrations" -v`
Expected: PASS

- [ ] **Step 5: Run all existing tests to verify no regressions**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -v`
Expected: All PASS

- [ ] **Step 6: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/migrate.go internal/config/migrate_test.go
git commit -m "feat(config): add workspace-to-volumes migration"
```

---

### Task 3: Update Validate() Error Message

**Files:**
- Modify: `internal/config/config.go:177-178`
- Modify: `internal/config/config_test.go` (update test expectation)

- [ ] **Step 1: Update error message in Validate()**

In `internal/config/config.go`, change line 178:

```go
// Before:
return errors.New("spec.workspace is removed; use spec.volumes (k8s Volume) and podTemplate.spec.containers[*].volumeMounts")

// After:
return errors.New("spec.workspace is removed; use spec.volumes (k8s Volume) and podTemplate.spec.containers[*].volumeMounts, or run \"okdev migrate\" to automatically update your config")
```

- [ ] **Step 2: Run existing tests to verify nothing broke**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run TestValidateRejectsLegacyWorkspace -v`
Expected: PASS (test only checks `err == nil`)

- [ ] **Step 3: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/config.go
git commit -m "fix(config): suggest okdev migrate in workspace validation error"
```

---

### Task 3b: Add Migration Hint to loadConfigAndNamespace

**Files:**
- Modify: `internal/cli/common.go:35-45`

All commands that load config (`up`, `ssh`, `sync`, `ports`, etc.) go through `loadConfigAndNamespace()` in `common.go`. When `config.Load()` fails with a validation error related to deprecated fields, we should print a visible hint.

- [ ] **Step 1: Add migration hint wrapper**

In `internal/config/config.go`, add a sentinel error type to identify migration-eligible errors:

```go
// MigrationEligibleError wraps validation errors that can be fixed by okdev migrate.
type MigrationEligibleError struct {
	Err error
}

func (e *MigrationEligibleError) Error() string { return e.Err.Error() }
func (e *MigrationEligibleError) Unwrap() error { return e.Err }
```

Update the workspace validation in `Validate()` to return this type:

```go
if d.Spec.Workspace != nil {
	return &MigrationEligibleError{Err: errors.New("spec.workspace is removed; use spec.volumes (k8s Volume) and podTemplate.spec.containers[*].volumeMounts, or run \"okdev migrate\" to automatically update your config")}
}
```

- [ ] **Step 2: Add hint in loadConfigAndNamespace**

In `internal/cli/common.go`, update `loadConfigAndNamespace` to detect the error type:

```go
func loadConfigAndNamespace(opts *Options) (*config.DevEnvironment, string, error) {
	path, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	done := announceConfigPath(path)
	cfg, path, err := config.Load(path)
	done(err == nil)
	if err != nil {
		var migErr *config.MigrationEligibleError
		if errors.As(err, &migErr) {
			fmt.Fprintf(os.Stderr, "\nHint: run \"okdev migrate\" to automatically fix this.\n")
		}
		return nil, "", err
	}
	applyConfigKubeContext(opts, cfg)
	ns := cfg.Spec.Namespace
	if opts.Namespace != "" {
		ns = opts.Namespace
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}
	return cfg, ns, nil
}
```

Add `"errors"` to the import block if not already present.

- [ ] **Step 3: Run tests**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./... -v`
Expected: All PASS

- [ ] **Step 4: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/config.go internal/cli/common.go
git commit -m "feat(cli): add visible migration hint when config load fails due to deprecated fields"
```

---

### Task 4: `okdev migrate` CLI Command

**Files:**
- Create: `internal/cli/migrate.go`
- Modify: `internal/cli/root.go:50` (add `newMigrateCmd`)

- [ ] **Step 1: Implement the migrate command**

Create `internal/cli/migrate.go`:

```go
package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
	"gopkg.in/yaml.v3"
)

func newMigrateCmd(opts *Options) *cobra.Command {
	var dryRun bool
	var noBackup bool

	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Migrate a .okdev.yaml config to the latest schema",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfgPath, err := config.ResolvePath(opts.ConfigPath)
			if err != nil {
				return err
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
	return cmd
}
```

Note: The spec says `--backup` (default true), but `--no-backup` (default false) is used here for better CLI ergonomics — the behavior is identical (backup is on by default).

- [ ] **Step 2: Register the command in root.go**

In `internal/cli/root.go`, add after line 50 (`newPruneCmd`):

```go
cmd.AddCommand(newMigrateCmd(opts))
```

- [ ] **Step 3: Build to verify compilation**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go build ./...`
Expected: Success

- [ ] **Step 4: Run all tests**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./...`
Expected: All PASS

- [ ] **Step 5: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/cli/migrate.go internal/cli/root.go
git commit -m "feat(cli): add okdev migrate command"
```

---

## Chunk 2: Template System Redesign

### Task 5: Create Embedded Template Files

**Files:**
- Create: `internal/config/templates/basic.yaml.tmpl`
- Create: `internal/config/templates/gpu.yaml.tmpl`
- Create: `internal/config/templates/llm-stack.yaml.tmpl`

- [ ] **Step 1: Create basic.yaml.tmpl**

Create `internal/config/templates/basic.yaml.tmpl`:

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  session:
    defaultNameTemplate: '{{`{{ .Repo }}-{{ .Branch }}-{{ .User }}`}}'
    ttlHours: {{ .TTLHours }}
    idleTimeoutMinutes: 120
    shareable: true
  sync:
    engine: syncthing
    syncthing:
      version: {{ .SyncthingVersion }}
      autoInstall: true
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
    exclude:
      - .git/
      - .venv/
      - node_modules/
    remoteExclude: []
  ports:
    - name: app
      local: 8080
      remote: 8080
  ssh:
    user: {{ .SSHUser }}
    remotePort: 22
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
  sidecar:
    image: {{ .SidecarImage }}
```

- [ ] **Step 2: Create gpu.yaml.tmpl**

Create `internal/config/templates/gpu.yaml.tmpl`:

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  session:
    defaultNameTemplate: '{{`{{ .Repo }}-{{ .Branch }}-{{ .User }}`}}'
    ttlHours: {{ .TTLHours }}
    idleTimeoutMinutes: 120
    shareable: true
  sync:
    engine: syncthing
    syncthing:
      version: {{ .SyncthingVersion }}
      autoInstall: true
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
    exclude:
      - .git/
      - .venv/
      - node_modules/
      - checkpoints/
      - data/
    remoteExclude: []
  ports:
    - name: api
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
  ssh:
    user: {{ .SSHUser }}
    remotePort: 22
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
  sidecar:
    image: {{ .SidecarImage }}
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .BaseImage }}
          command: ["sleep", "infinity"]
          resources:
            requests:
              cpu: "8"
              memory: 32Gi
              nvidia.com/gpu: "{{ .GPUCount }}"
            limits:
              cpu: "16"
              memory: 64Gi
              nvidia.com/gpu: "{{ .GPUCount }}"
          volumeMounts:
            - name: workspace
              mountPath: /workspace
      volumes:
        - name: workspace
          persistentVolumeClaim:
            claimName: okdev-workspace
```

- [ ] **Step 3: Create llm-stack.yaml.tmpl**

Create `internal/config/templates/llm-stack.yaml.tmpl`:

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  session:
    defaultNameTemplate: '{{`{{ .Repo }}-{{ .Branch }}-{{ .User }}`}}'
    ttlHours: {{ .TTLHours }}
    idleTimeoutMinutes: 120
    shareable: true
  sync:
    engine: syncthing
    syncthing:
      version: {{ .SyncthingVersion }}
      autoInstall: true
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
    exclude:
      - .git/
      - .venv/
      - node_modules/
      - checkpoints/
      - data/
    remoteExclude: []
  ports:
    - name: app
      local: 8080
      remote: 8080
    - name: redis
      local: 6379
      remote: 6379
    - name: qdrant
      local: 6333
      remote: 6333
  ssh:
    user: {{ .SSHUser }}
    remotePort: 22
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
  sidecar:
    image: {{ .SidecarImage }}
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .BaseImage }}
          command: ["sleep", "infinity"]
          env:
            - name: REDIS_URL
              value: redis://127.0.0.1:6379
            - name: QDRANT_URL
              value: http://127.0.0.1:6333
          resources:
            requests:
              cpu: "8"
              memory: 32Gi
              nvidia.com/gpu: "{{ .GPUCount }}"
            limits:
              cpu: "16"
              memory: 64Gi
              nvidia.com/gpu: "{{ .GPUCount }}"
          volumeMounts:
            - name: workspace
              mountPath: /workspace
        - name: redis
          image: redis:7-alpine
          args: ["--save", "", "--appendonly", "no"]
          ports:
            - containerPort: 6379
        - name: qdrant
          image: qdrant/qdrant:v1.13.2
          ports:
            - containerPort: 6333
      volumes:
        - name: workspace
          persistentVolumeClaim:
            claimName: okdev-workspace
```

- [ ] **Step 4: Commit template files**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/templates/
git commit -m "feat(config): add embedded yaml template files"
```

---

### Task 6: Rewrite template.go with TemplateVars and Resolution

**Files:**
- Rewrite: `internal/config/template.go`
- Rewrite: `internal/config/template_test.go`

- [ ] **Step 1: Write failing test for TemplateVars defaults and RenderTemplate**

**Full replacement** of `internal/config/template_test.go` (the old tests for `TemplateByName` are removed since that function no longer exists; `writeFile` helper is available from `loader_test.go` in the same package):

```go
package config

import (
	"strings"
	"testing"
)

func TestDefaultTemplateVars(t *testing.T) {
	vars := NewTemplateVars()
	if vars.Namespace != "default" {
		t.Fatalf("expected default namespace, got %q", vars.Namespace)
	}
	if vars.SSHUser != "root" {
		t.Fatalf("expected root ssh user, got %q", vars.SSHUser)
	}
	if vars.SyncLocal != "." {
		t.Fatalf("expected . sync local, got %q", vars.SyncLocal)
	}
	if vars.SyncRemote != "/workspace" {
		t.Fatalf("expected /workspace sync remote, got %q", vars.SyncRemote)
	}
}

func TestRenderBuiltinBasic(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "test-project"
	out, err := RenderTemplate("basic", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: test-project") {
		t.Fatal("expected rendered name in output")
	}
	if !strings.Contains(out, "namespace: default") {
		t.Fatal("expected namespace in output")
	}
	// Verify runtime template syntax is preserved literally
	if !strings.Contains(out, "{{ .Repo }}-{{ .Branch }}-{{ .User }}") {
		t.Fatal("expected session name template to be preserved literally")
	}
}

func TestResolveTemplateBuiltinName(t *testing.T) {
	content, err := ResolveTemplate("basic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "{{ .Name }}") {
		t.Fatal("expected template variable in raw content")
	}
}

func TestResolveTemplateFilePath(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := tmp + "/custom.yaml.tmpl"
	writeFile(t, tmplPath, "name: {{ .Name }}")
	content, err := ResolveTemplate(tmplPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "name: {{ .Name }}" {
		t.Fatalf("unexpected content: %q", content)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run "TestDefaultTemplateVars|TestRenderBuiltin|TestResolveTemplate" -v`
Expected: FAIL — new functions undefined

- [ ] **Step 3: Rewrite template.go**

Replace `internal/config/template.go` with:

```go
package config

import (
	"bytes"
	"embed"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/template"
	"time"
)

//go:embed templates/*.yaml.tmpl
var embeddedTemplates embed.FS

// builtinNames maps template names to their embedded file paths.
var builtinNames = map[string]string{
	"basic":           "templates/basic.yaml.tmpl",
	"gpu":             "templates/gpu.yaml.tmpl",
	"llm-gpu":         "templates/gpu.yaml.tmpl",
	"llm-stack":       "templates/llm-stack.yaml.tmpl",
	"multi-container": "templates/llm-stack.yaml.tmpl",
}

// TemplateVars holds all variables available to templates.
type TemplateVars struct {
	Name              string
	Namespace         string
	SidecarImage      string
	SyncthingVersion  string
	SyncLocal         string
	SyncRemote        string
	SSHUser           string
	Ports             []PortVar
	BaseImage         string
	GPUCount          string
	TTLHours          int
}

type PortVar struct {
	Name   string
	Local  int
	Remote int
}

// NewTemplateVars returns TemplateVars with sensible defaults.
func NewTemplateVars() *TemplateVars {
	return &TemplateVars{
		Name:             "",
		Namespace:        "default",
		SidecarImage:     DefaultSidecarImage,
		SyncthingVersion: DefaultSyncthingVersion,
		SyncLocal:        ".",
		SyncRemote:       "/workspace",
		SSHUser:          "root",
		BaseImage:        "nvidia/cuda:12.4.1-devel-ubuntu22.04",
		GPUCount:         "1",
		TTLHours:         72,
	}
}

// ResolveTemplate loads raw template content from a built-in name, file path, or URL.
// Resolution: if ref contains '/' or a known extension (.yaml, .yml, .tmpl), try file first.
// Otherwise check built-in names. If neither matches, treat as URL.
func ResolveTemplate(ref string) (string, error) {
	hasPathIndicator := strings.Contains(ref, "/") ||
		strings.HasSuffix(ref, ".yaml") ||
		strings.HasSuffix(ref, ".yml") ||
		strings.HasSuffix(ref, ".tmpl")

	if hasPathIndicator {
		if content, err := os.ReadFile(ref); err == nil {
			return string(content), nil
		}
	}

	// Check built-in
	if path, ok := builtinNames[ref]; ok {
		content, err := embeddedTemplates.ReadFile(path)
		if err != nil {
			return "", fmt.Errorf("read embedded template %q: %w", ref, err)
		}
		return string(content), nil
	}

	if !hasPathIndicator {
		// Try as file anyway (no extension, no slash, but not a built-in name)
		if content, err := os.ReadFile(ref); err == nil {
			return string(content), nil
		}
	}

	// Treat as URL
	return fetchTemplateURL(ref)
}

func fetchTemplateURL(url string) (string, error) {
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return "", fmt.Errorf("fetch template from %q: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch template from %q: HTTP %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20)) // 1MB limit
	if err != nil {
		return "", fmt.Errorf("read template from %q: %w", url, err)
	}
	return string(body), nil
}

// RenderTemplate resolves a template by ref and renders it with the given vars.
func RenderTemplate(ref string, vars *TemplateVars) (string, error) {
	raw, err := ResolveTemplate(ref)
	if err != nil {
		return "", err
	}

	tmpl, err := template.New("okdev").Parse(raw)
	if err != nil {
		return "", fmt.Errorf("parse template: %w", err)
	}

	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, vars); err != nil {
		return "", fmt.Errorf("render template: %w", err)
	}
	return buf.String(), nil
}

// BuiltinTemplateNames returns the list of available built-in template names.
func BuiltinTemplateNames() []string {
	seen := map[string]bool{}
	var names []string
	for name := range builtinNames {
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}
```

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/config/ -run "TestDefaultTemplateVars|TestRenderBuiltin|TestResolveTemplate" -v`
Expected: PASS

- [ ] **Step 5: Rewrite init.go simultaneously to avoid broken intermediate state**

Since `init.go` calls `config.TemplateByName()` which is being removed, rewrite `init.go` in the same step. Replace `internal/cli/init.go` with:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func newInitCmd(opts *Options) *cobra.Command {
	var force bool
	var templateRef string
	var yes bool
	var nameOverride string
	var nsOverride string

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Create a starter .okdev.yaml config",
		RunE: func(cmd *cobra.Command, args []string) error {
			target := config.DefaultFile
			if opts.ConfigPath != "" {
				target = opts.ConfigPath
			}

			abs, err := filepath.Abs(target)
			if err != nil {
				return fmt.Errorf("resolve output path %q: %w", target, err)
			}

			if _, err := os.Stat(abs); err == nil && !force {
				return fmt.Errorf("config already exists at %q (use --force to overwrite)", abs)
			}

			vars := config.NewTemplateVars()
			if nameOverride != "" {
				vars.Name = nameOverride
			}
			if nsOverride != "" {
				vars.Namespace = nsOverride
			}
			if vars.Name == "" {
				wd, _ := os.Getwd()
				if wd != "" {
					vars.Name = filepath.Base(wd)
				} else {
					vars.Name = "my-project"
				}
			}

			// TODO: interactive prompts will be added in Task 8
			// For now, non-interactive only (--yes is implicit until prompts are wired)

			rendered, err := config.RenderTemplate(templateRef, vars)
			if err != nil {
				return err
			}

			if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
				return fmt.Errorf("create parent directory: %w", err)
			}
			if err := os.WriteFile(abs, []byte(rendered), 0o644); err != nil {
				return fmt.Errorf("write config %q: %w", abs, err)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Wrote %s\n", abs)
			return nil
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Overwrite an existing config file")
	cmd.Flags().StringVar(&templateRef, "template", "basic", "Template: built-in name, file path, or URL")
	cmd.Flags().BoolVar(&yes, "yes", false, "Non-interactive mode, accept all defaults")
	cmd.Flags().StringVar(&nameOverride, "name", "", "Environment name")
	cmd.Flags().StringVar(&nsOverride, "namespace", "", "Namespace")
	return cmd
}
```

Note: The old `var DefaultTemplate` export is removed. No other files in the codebase reference it.

- [ ] **Step 6: Build to verify compilation**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go build ./...`
Expected: Success

- [ ] **Step 7: Run all tests**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./... -v`
Expected: All PASS

- [ ] **Step 8: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/config/template.go internal/config/template_test.go internal/cli/init.go
git commit -m "feat(config): rewrite template system with text/template and embed"
```

---

## Chunk 3: Interactive Init

### Task 7: Add Prompt Library Dependency

**Files:**
- Modify: `go.mod`, `go.sum`

- [ ] **Step 1: Add charmbracelet/huh dependency**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
go get github.com/charmbracelet/huh@latest
```

If `charmbracelet/huh` is too heavy, use `github.com/AlecAivazis/survey/v2` instead:

```bash
go get github.com/AlecAivazis/survey/v2@latest
```

- [ ] **Step 2: Commit dependency update**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add go.mod go.sum
git commit -m "deps: add prompt library for interactive init"
```

---

### Task 8: Create prompt.go in internal/cli

**Files:**
- Create: `internal/cli/prompt.go`
- Create: `internal/cli/prompt_test.go`

- [ ] **Step 1: Write failing test for prompt defaults**

In `internal/cli/prompt_test.go`:

```go
package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestApplyFlagOverrides(t *testing.T) {
	vars := config.NewTemplateVars()
	overrides := InitOverrides{
		Name:      "my-env",
		Namespace: "staging",
	}
	applyOverrides(vars, overrides)

	if vars.Name != "my-env" {
		t.Fatalf("expected name my-env, got %q", vars.Name)
	}
	if vars.Namespace != "staging" {
		t.Fatalf("expected namespace staging, got %q", vars.Namespace)
	}
}

func TestApplyFlagOverridesEmptyNoChange(t *testing.T) {
	vars := config.NewTemplateVars()
	vars.Name = "original"
	overrides := InitOverrides{}
	applyOverrides(vars, overrides)

	if vars.Name != "original" {
		t.Fatalf("expected name to remain original, got %q", vars.Name)
	}
}
```

- [ ] **Step 2: Run tests to verify they fail**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/cli/ -run TestApplyFlag -v`
Expected: FAIL — `InitOverrides` and `applyOverrides` undefined

- [ ] **Step 3: Implement prompt.go**

Create `internal/cli/prompt.go`:

```go
package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/acmore/okdev/internal/config"
)

// InitOverrides holds flag-provided values that skip prompting.
type InitOverrides struct {
	Name      string
	Namespace string
}

// applyOverrides applies non-empty flag values to template vars.
func applyOverrides(vars *config.TemplateVars, o InitOverrides) {
	if o.Name != "" {
		vars.Name = o.Name
	}
	if o.Namespace != "" {
		vars.Namespace = o.Namespace
	}
}

// detectDefaultName returns a sensible default for the environment name
// based on the current directory (git repo basename).
func detectDefaultName() string {
	wd, err := os.Getwd()
	if err != nil {
		return "my-project"
	}
	return filepath.Base(wd)
}

// promptInteractive runs interactive prompts to fill in template vars.
// Only prompts for values not already set by flags.
// When nonInteractive is true, uses defaults without prompting.
func promptInteractive(vars *config.TemplateVars, nonInteractive bool) error {
	if vars.Name == "" {
		vars.Name = detectDefaultName()
	}
	if nonInteractive {
		return nil
	}

	// Interactive prompts using the chosen prompt library.
	// Each prompt shows the current default and allows the user to accept or override.
	var input string

	input = promptString("Environment name", vars.Name)
	if input != "" {
		vars.Name = input
	}

	input = promptString("Namespace", vars.Namespace)
	if input != "" {
		vars.Namespace = input
	}

	input = promptString("Sidecar image", vars.SidecarImage)
	if input != "" {
		vars.SidecarImage = input
	}

	input = promptString("Sync local path", vars.SyncLocal)
	if input != "" {
		vars.SyncLocal = input
	}

	input = promptString("Sync remote path", vars.SyncRemote)
	if input != "" {
		vars.SyncRemote = input
	}

	input = promptString("SSH user", vars.SSHUser)
	if input != "" {
		vars.SSHUser = input
	}

	return nil
}

// promptString prints a prompt with a default and reads user input.
// Returns empty string if user accepts default (just presses Enter).
func promptString(label, defaultVal string) string {
	fmt.Printf("? %s: (%s) ", label, defaultVal)
	var input string
	fmt.Scanln(&input)
	return input
}
```

Note: The prompt implementation above uses basic `fmt.Scanln` for simplicity. Replace with the chosen prompt library (charmbracelet/huh or survey) for better UX (arrow key selection, validation, etc.) during implementation. The interface (`promptInteractive`) stays the same.

- [ ] **Step 4: Run tests to verify they pass**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./internal/cli/ -run TestApplyFlag -v`
Expected: PASS

- [ ] **Step 5: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/cli/prompt.go internal/cli/prompt_test.go
git commit -m "feat(cli): add prompt system for interactive init"
```

---

### Task 9: Wire Prompts Into init.go

**Files:**
- Modify: `internal/cli/init.go`

- [ ] **Step 1: Update init.go to use prompt system**

In `internal/cli/init.go`, replace the inline name/namespace override logic with `InitOverrides` and `promptInteractive` from `prompt.go`:

```go
// Replace the vars population block in RunE with:
vars := config.NewTemplateVars()
overrides := InitOverrides{Name: nameOverride, Namespace: nsOverride}
applyOverrides(vars, overrides)

if err := promptInteractive(vars, yes); err != nil {
	return err
}
```

Remove the inline `wd` / `filepath.Base` fallback since `promptInteractive` handles the default name via `detectDefaultName()`.

- [ ] **Step 2: Build to verify compilation**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go build ./...`
Expected: Success

- [ ] **Step 3: Run all tests**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./...`
Expected: All PASS

- [ ] **Step 4: Commit**

```bash
cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init
git add internal/cli/init.go
git commit -m "feat(cli): wire interactive prompts into init command"
```

---

### Task 10: Final Integration Test and Cleanup

**Files:**
- All files from previous tasks

- [ ] **Step 1: Run full test suite**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go test ./... -v`
Expected: All PASS

- [ ] **Step 2: Run go vet and build**

Run: `cd /Users/acmore/workspace/okdev/.worktrees/migrate-and-init && go vet ./... && go build ./...`
Expected: No errors

- [ ] **Step 3: Manual smoke test -- init**

```bash
cd /tmp && mkdir okdev-test && cd okdev-test
/Users/acmore/workspace/okdev/.worktrees/migrate-and-init/cmd/okdev/okdev init --yes --name smoke-test
cat .okdev.yaml
```

Verify the output is valid YAML with `name: smoke-test`.

- [ ] **Step 4: Manual smoke test -- migrate (dry-run)**

Create a legacy config and test migration:

```bash
cd /tmp/okdev-test
cat > .okdev.yaml << 'EOF'
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: legacy-test
spec:
  namespace: default
  workspace:
    mountPath: /code
    pvc:
      claimName: my-pvc
      size: 50Gi
  sidecar:
    image: ghcr.io/acmore/okdev:edge
EOF
/Users/acmore/workspace/okdev/.worktrees/migrate-and-init/cmd/okdev/okdev migrate --dry-run
```

Verify output shows the migration applied and the transformed YAML.

- [ ] **Step 5: Cleanup temp files**

```bash
rm -rf /tmp/okdev-test
```
