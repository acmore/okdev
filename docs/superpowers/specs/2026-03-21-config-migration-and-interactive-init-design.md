# Config Migration and Interactive Init Design

## Problem

1. Users with existing `.okdev.yaml` files containing deprecated fields (e.g., `spec.workspace`) get hard errors with no automated fix path.
2. `okdev init` is non-interactive and uses hardcoded `fmt.Sprintf` templates that are not customizable.

## Decisions

| Question | Decision |
|----------|----------|
| Migration trigger | Explicit `okdev migrate` command |
| Migration behavior | Best-effort with YAML comment annotations for ambiguous parts |
| Migration scope | Content-only (no file location changes) |
| Init interactivity | Interactive by default, `--yes` to skip |
| Template system | User-provided via path/URL, built-in ones by name |
| Overall approach | Unified migration registry + Go `text/template` engine |

---

## 1. Migration System

### `okdev migrate` Command

```
okdev migrate [flags]
  -c, --config <path>    Config file to migrate (uses standard discovery if omitted)
      --dry-run           Print migrated config to stdout without writing
      --backup            Save original as .okdev.yaml.bak before overwriting (default: true)
```

### Migration Registry

A chain of named migration functions, each targeting a specific transformation:

```go
type Migration struct {
    Name        string
    Description string
    Applies     func(node *yaml.Node) bool
    Transform   func(node *yaml.Node) ([]string, error) // returns warnings
}
```

Migrations are registered in order and run sequentially. Each migration:

1. Checks if it applies (e.g., does `spec.workspace` exist?)
2. Transforms the YAML node tree
3. Returns warnings for anything that couldn't be auto-resolved

### YAML Node Manipulation

Use `gopkg.in/yaml.v3` node API for the migrate command specifically. This preserves:

- User comments
- Key ordering
- Formatting/indentation

The rest of the codebase continues using struct-based parsing via `sigs.k8s.io/yaml`.

**Round-trip safety:** Since `sigs.k8s.io/yaml` (used for loading) and `gopkg.in/yaml.v3` (used for migration) have subtly different serialization behaviors (whitespace, quoting, null handling), the migrated output must be integration-tested to confirm it round-trips cleanly through `sigs.k8s.io/yaml` unmarshaling. Migration tests should load the migrated YAML back through the standard `config.Load()` path and validate the result.

### Idempotency

All migrations must be idempotent. Running `okdev migrate` twice on the same file must produce the same output. This is enforced by the `Applies()` check -- once a migration has transformed the config, `Applies()` should return false on subsequent runs. No version tracking marker is needed; the structure of the YAML itself is the version indicator.

### Ambiguity Handling

When a migration can't fully resolve a field, it:

1. Inserts a YAML comment at the relevant location (e.g., `# TODO: review storageClassName`)
2. Adds the warning to the summary printed after migration

Example output:

```
okdev migrate
 workspace-to-volumes: migrated spec.workspace to spec.volumes + podTemplate
  Warning: Review storageClassName in spec.volumes[0] -- was previously inferred
  Warning: Check volumeMount path matches your workflow

Wrote migrated config to .okdev.yaml (backup: .okdev.yaml.bak)
```

### Current Migrations

One migration to start:

- **workspace-to-volumes**: Transforms `spec.workspace` into `spec.volumes` + `spec.podTemplate.spec.containers[*].volumeMounts`.

  Expected `workspace` sub-keys:
  - `mountPath` (string) → becomes `volumeMounts[0].mountPath`
  - `pvc.claimName` (string) → becomes `volumes[0].persistentVolumeClaim.claimName`
  - `pvc.size` (string) → becomes `volumes[0].persistentVolumeClaim.resources.requests.storage`
  - `pvc.storageClassName` (string) → becomes `volumes[0].persistentVolumeClaim.storageClassName`

  Since `pvc` is `map[string]string`, unexpected keys are preserved as YAML comments with a warning (e.g., `# TODO: unknown workspace.pvc key "foo" = "bar" -- review manually`).

New migrations are added by appending to the registry as the schema evolves.

---

## 2. Template System Redesign

### Template Resolution

`--template` accepts three forms:

1. **Built-in name**: `--template basic` resolves to embedded template
2. **Local path**: `--template ./my-template.yaml` reads from disk
3. **URL**: `--template https://example.com/template.yaml` fetches remotely

Resolution order: if the value contains a path separator (`/`) or file extension (`.yaml`, `.yml`, `.tmpl`), check as file path first; otherwise check if it's a built-in name. If neither matches, treat as URL. This avoids the footgun where a local file named `basic` is shadowed by the built-in.

### Template Format

Templates are standard `.okdev.yaml` files with Go `text/template` variables:

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace | default "default" }}
  sidecar:
    image: {{ .SidecarImage }}
  session:
    defaultNameTemplate: '{{`{{ .Repo }}-{{ .Branch }}-{{ .User }}`}}'
  sync:
    engine: syncthing
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
  ssh:
    user: {{ .SSHUser | default "root" }}
  {{- if .Ports }}
  ports:
    {{- range .Ports }}
    - name: {{ .Name }}
      local: {{ .Local }}
      remote: {{ .Remote }}
    {{- end }}
  {{- end }}
```

**Template escaping:** The existing `session.defaultNameTemplate` field uses `{{ .Repo }}`, `{{ .Branch }}`, `{{ .User }}` syntax that is resolved at session runtime, not at init time. In `.yaml.tmpl` files, these must be escaped using Go's backtick-raw syntax: `` {{` + "`" + `{{ .Repo }}` + "`" + `}} `` so they pass through `text/template` rendering as literal strings.

### Built-in Templates

The current three templates (`basic`, `gpu`, `llm-stack`) are converted from hardcoded `fmt.Sprintf` strings to embedded `.yaml.tmpl` files under `internal/config/templates/`, using `//go:embed` directives. This makes them readable examples for users authoring custom templates.

### Template Variables

```go
type TemplateVars struct {
    // Common (all templates)
    Name          string   // metadata.name (default: repo basename)
    Namespace     string   // default: "default"
    SidecarImage  string   // default: version-derived
    SyncLocal     string   // default: "."
    SyncRemote    string   // default: "/workspace"
    SSHUser       string   // default: "root"
    Ports         []PortVar

    // GPU template
    BaseImage     string   // podTemplate container image (default: "nvidia/cuda:12.4.1-devel-ubuntu22.04")
    GPUCount      string   // nvidia.com/gpu resource limit (default: "1")

    // Session
    TTLHours      int      // session.ttlHours (default: 0, meaning no TTL)
}
```

Template-specific variables are only prompted when the selected template references them. Built-in templates define which variables they use. For user-provided templates, unused variables are rendered as their zero values -- the template author controls what variables appear via standard Go template syntax.

The prompt system does not auto-discover variables from custom templates. Custom template authors are expected to render the template with `--yes` (defaults) or provide values via flags. Interactive prompts are limited to the known `TemplateVars` fields.

---

## 3. Interactive `okdev init` Flow

### Default Behavior (Interactive)

When the user runs `okdev init` without `--yes`:

```
$ okdev init

? Template: (basic) [basic / gpu / llm-stack / path or URL]
? Environment name: (my-repo)
? Namespace: (default)
? Sidecar image: (ghcr.io/acmore/okdev:v0.2.1)
? Sync local path: (.)
? Sync remote path: (/workspace)
? SSH user: (root)
? Add port forwards? (y/N)

Wrote .okdev.yaml
```

Each prompt shows the default in parentheses. Pressing Enter accepts the default. The prompts are derived from the template's variables -- a custom template with different variables would produce different prompts.

Template is asked first. The selected template determines which follow-up questions are asked (e.g., a GPU template might ask about GPU count).

### Non-Interactive Mode

`okdev init --yes` skips all prompts and uses defaults. Equivalent to the current behavior.

Values can also be passed as flags to override specific defaults without prompts:

```
okdev init --yes --namespace staging --name my-env
```

### Flag Summary

```
okdev init [flags]
  -c, --config <path>        Output path (default: .okdev.yaml)
      --template <name|path>  Template to use (default: basic)
      --force                 Overwrite existing config
      --yes                   Non-interactive, accept all defaults
      --name <string>         Environment name
      --namespace <string>    Namespace
```

---

## 4. Integration

### Separation of Concerns

- `okdev migrate` transforms existing configs. Never prompts for new values; only restructures what's there.
- `okdev init` creates new configs. Interactive prompts, template rendering.
- No overlap.

### Validation Integration

The existing `Validate()` error message for deprecated fields is updated to suggest running `okdev migrate`:

```
Error: spec.workspace is no longer supported.
Use spec.volumes (k8s Volume) and podTemplate.spec.containers[*].volumeMounts instead,
or run "okdev migrate" to automatically update your config.
```

The existing manual-fix guidance is preserved, with the `okdev migrate` suggestion appended.

Additionally, `loadConfigAndNamespace()` in `internal/cli/common.go` (the shared config-loading path for all commands like `up`, `ssh`, `sync`, `ports`) detects migration-eligible validation errors and prints a visible hint to stderr:

```
Hint: run "okdev migrate" to automatically fix this.
```

This uses a `MigrationEligibleError` sentinel type in the config package so the CLI layer can distinguish migration-fixable errors from other validation failures.

### Package Layout

```
internal/config/
  config.go          # Structs, defaults, validation (existing)
  loader.go          # Load/discovery (existing)
  migrate.go         # Migration registry + migrate logic (new)
  migrate_test.go    # (new)
  template.go        # TemplateVars, rendering, resolution (rewritten)
  template_test.go   # (rewritten)
  templates/
    basic.yaml.tmpl      # //go:embed
    gpu.yaml.tmpl        # //go:embed
    llm-stack.yaml.tmpl  # //go:embed

internal/cli/
  init.go            # Updated to use new template + prompt system
  prompt.go          # Interactive prompts for init (new)
  prompt_test.go     # (new)
  migrate.go         # New subcommand wiring (new)
```

Prompt logic lives in `internal/cli/` (not `internal/config/`) to keep the config package free of terminal/tty dependencies.

### Dependencies

- Interactive prompts: `github.com/AlecAivazis/survey/v2` or `github.com/charmbracelet/huh`
- YAML node manipulation: `gopkg.in/yaml.v3` (already available)
- URL template fetching: HTTP GET with 30s timeout, fail on non-200, no caching, TLS verification enabled
