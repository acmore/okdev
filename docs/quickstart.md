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
# GPU/LLM-focused scaffold
okdev init --template gpu
```

This generates `.okdev.yaml` in the current directory. See [Config Manifest](config-manifest.md) for the full field reference and examples.

okdev discovers configuration in this order:

1. `-c, --config <path>` flag
2. `.okdev.yaml`
3. `okdev.yaml`

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

`okdev` does not have first-class coding-agent commands yet, but the SSH workflow already works with remote development tools that can connect over SSH.

Examples:

```bash
# Open a plain remote shell and install tools in the container
ssh okdev-<session>

# Then inside the container, install or run the tools you want
codex
claude
```

Cursor and similar editors can use the managed `okdev-<session>` SSH host directly for remote development. The generated SSH host entry bypasses tmux by default, which is usually the right behavior for editor-driven SSH sessions.

Typical flow:

1. `okdev up`
2. Open your editor's SSH remote connection to `okdev-<session>`
3. Install `codex`, `claude`, or other CLI tools inside the container if needed
4. Use the editor remote session or `ssh okdev-<session>` as your entry point

Use `okdev ssh` when you want the tmux-backed interactive shell managed by okdev. Use `ssh okdev-<session>` when you want a plain SSH session for editors, remote IDEs, or direct tool execution.

## File Sync

`okdev up` starts bidirectional Syncthing sync automatically. For manual control:

```bash
# Foreground sync (useful for troubleshooting)
okdev sync

# Detached background sync
okdev sync --background

# One-way sync (local → remote or remote → local)
okdev sync --mode up
okdev sync --mode down
```

Local Syncthing is auto-installed when `sync.engine=syncthing`.

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
