# Quickstart

## Prerequisites

- Go 1.21+
- `kubectl` configured to a cluster
- Namespace permissions for Pod/PVC create/update/delete

## Install

**Prebuilt binary:**

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
okdev version
```

**From source:**

```bash
go build -o bin/okdev ./cmd/okdev
```

## Create a Manifest

```bash
okdev init
# Job scaffold with starter manifest
okdev init --workload job
# Generic deployment-style scaffold
okdev init --workload generic --generic-preset deployment
# Go project with a Go-oriented local sync ignore preset
okdev init --template basic --stignore-preset go
```

This generates `.okdev.yaml` in the current directory for simple pod setups.
When `okdev init` also scaffolds workload files under `.okdev/` such as `job.yaml`, it writes the config as `.okdev/okdev.yaml` instead. See [Config Manifest](config-manifest.md) for the full field reference and examples.

For built-in templates, `okdev init` also writes a starter local `.stignore` file for the initialized sync root. Use `--stignore-preset` to override that starter with a project-oriented preset like `python`, `node`, `go`, or `rust`.
When `--stignore-preset` is omitted, `okdev init` will try to detect a preset from common repo markers like `go.mod`, `package.json`, `Cargo.toml`, or `pyproject.toml`.
`okdev init --template` also supports user templates from `~/.okdev/templates/` by stem name, in addition to file paths and URLs.

okdev discovers configuration in this order:

1. `-c, --config <path>` flag
2. `.okdev/okdev.yaml`
3. `.okdev.yaml`
4. `okdev.yaml`

## Start a Session

```bash
okdev up
```

This creates the dev pod, configures SSH, starts file sync, and sets up port forwarding. It exits after setup — no long-running foreground process.

Specifically, `okdev up`:

- ensures Pod and PVC state in the cluster
- writes a managed `~/.ssh/config` host entry (`okdev-<session>`)
- starts `okdev-sshd` inside the dev container
- starts background Syncthing sync (bidirectional)
- configures port forwarding from `spec.ports`

**Named sessions** let you run multiple environments side by side:

```bash
okdev up --session serving-main-alice
```

**Disable tmux** for a plain SSH shell:

```bash
okdev up --no-tmux
```

## Connect

```bash
# Interactive shell (tmux-backed by default)
okdev ssh

# Or use standard SSH directly
ssh okdev-<session>

# Force TTY for tmux over standard SSH
ssh -tt okdev-<session>
```

## Remote Coding Tools

`okdev` now has first-class config for supported coding-agent CLIs and setup-time auth staging from local auth files for dedicated sessions.

Examples:

```bash
# Open a plain remote shell and install tools in the container
ssh okdev-<session>

# Check configured agent install/auth status
okdev agent list

# Then inside the container, run the tools you want
codex
claude
```

Cursor and similar editors can use the managed `okdev-<session>` SSH host directly for remote development. The generated SSH host entry bypasses tmux by default, which is usually the right behavior for editor-driven SSH sessions.

Typical flow:

1. `okdev up`
2. Optional: configure `spec.agents` and let `okdev up` perform best-effort CLI install checks plus local auth-file staging
3. Run `okdev agent list` to confirm install/auth status
4. Open your editor's SSH remote connection to `okdev-<session>`
5. Use the editor remote session or `ssh okdev-<session>` as your entry point

Current scope:

- `okdev up` checks/install configured `claude-code`, `codex`, `gemini`, and `opencode` CLIs when possible, including best-effort Node/npm bootstrap via `nvm` when the image has `bash` and `curl`
- `okdev up` stages configured local auth files into reserved runtime paths for dedicated sessions
- Codex defaults to `~/.codex/auth.json`, but you can point it at a company-specific file with `spec.agents[].auth.localPath`
- `okdev down` cleans that staged runtime auth up before session deletion when the target is still reachable
- `okdev agent list` reports configured agents, install status, and auth staging status
- users still launch agent CLIs manually through `okdev ssh`, plain SSH, or editor remote sessions
- shareable sessions skip auth staging and warn instead

Use `okdev ssh` when you want the tmux-backed interactive shell managed by okdev. Use `ssh okdev-<session>` when you want a plain SSH session for editors, remote IDEs, or direct tool execution.

## File Sync

`okdev up` starts bidirectional Syncthing sync automatically. For manual control:

```bash
# Detached background sync (default)
okdev sync

# Foreground sync (useful for troubleshooting)
okdev sync --foreground

# One-way sync (local → remote or remote → local)
okdev sync --mode up
okdev sync --mode down
```

Local Syncthing is auto-installed when `sync.engine=syncthing`.
If your local filesystem watcher occasionally misses changes, set `spec.sync.syncthing.rescanIntervalSeconds` to a lower value such as `300` for a faster fallback rescan.

## Port Forwarding

Ports defined in `spec.ports` are forwarded as SSH `LocalForward` entries. To reconcile ports after manifest changes:

```bash
okdev ports
```

## Multi-Session Operations

```bash
okdev list
okdev use serving-main-alice
okdev status --all
okdev list --all-users    # cross-owner visibility
```

## Dry Run

Preview what okdev would do without modifying the cluster:

```bash
okdev up --dry-run
okdev sync --dry-run
okdev down --dry-run
```

For controller-backed workloads, `okdev up` reuses an existing session workload by default. Use `okdev up --reconcile` only when you want to reapply the workload manifest explicitly.

## Teardown

```bash
okdev down
okdev prune --ttl-hours 72
```

## Kubernetes Context

okdev resolves the target cluster in this order:

1. `--context` CLI flag (highest priority)
2. `spec.kubeContext` in the manifest
3. Active context from your kubeconfig

## SSH Details

- SSH connects to the `dev` container via `okdev-sshd` on port 2222.
- `okdev ssh` uses tmux by default with a built-in okdev profile (history, mouse, vi-copy, status bar) using the standard `Ctrl-b` prefix.
- The generated `ssh okdev-<session>` host entry bypasses tmux by default for a plain shell.
- Use `okdev ssh --no-tmux` to bypass tmux for a single `okdev ssh` connection, or set `spec.ssh.persistentSession: false` to disable it globally.
- The managed SSH host entry uses tight proxy keepalive settings so hung sessions fail fast instead of freezing.
- Tune keepalive with `spec.ssh.keepAliveIntervalSeconds` (default `30`) and `spec.ssh.keepAliveTimeoutSeconds` (default `90`, must be >= interval).
- Proxy diagnostics are written to `~/.okdev/logs/okdev.log`, not the SSH session.
- Local runtime state and logs live under `~/.okdev/`.
