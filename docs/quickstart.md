# Quickstart

## Prerequisites

- Go 1.21+
- `kubectl` configured to a cluster
- Namespace permissions for Pod/PVC create/update/delete

## Build From Source

```bash
go build -o bin/okdev ./cmd/okdev
```

## Install Prebuilt Binary

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
okdev version
```

## Initialize Configuration

```bash
./bin/okdev init
# Optional: GPU/LLM-focused scaffold
./bin/okdev init --template gpu
```

Configuration discovery order:
1. `-c, --config <path>`
2. `.okdev.yaml`
3. `okdev.yaml`

Kubernetes context precedence:
1. `--context`
2. `spec.kubeContext` in manifest
3. active kube context from your kubeconfig

## Start or Resume Session

```bash
./bin/okdev up
```

Use an explicit session name when running multiple environments for the same repo:

```bash
./bin/okdev up --session serving-main-alice
```

Enable persistent interactive shells backed by tmux:

```bash
./bin/okdev up --tmux
```

`okdev up` performs one-step setup:
- ensures Pod/PVC state
- writes managed `~/.ssh/config` host entry (`okdev-<session>`)
- starts managed SSH/forwarding bootstrap
- starts background Syncthing sync when enabled
- exits after setup (no attach mode)

## Access and Data Movement

```bash
./bin/okdev connect
./bin/okdev ssh --setup-key
./bin/okdev ssh
./bin/okdev sync --mode up
./bin/okdev ports

# Foreground continuous sync (Syncthing engine)
./bin/okdev sync

# Detached sync process
./bin/okdev sync --background
```

Notes:
- Local Syncthing is auto-installed when `sync.engine=syncthing`.
- Default sidecar image follows binary version: `ghcr.io/<owner>/okdev:<okdev-version>` (`edge` for dev builds).
- `spec.sync.exclude` applies local ignore patterns; `spec.sync.remoteExclude` applies remote-only ignores.
- SSH target is always the merged `okdev-sidecar` container (`sshd` on `22`).
- `spec.ports` are rendered as SSH `LocalForward` entries.
- Local runtime state/logs are stored under `~/.okdev/` (not in the project working directory).
- Use `ssh okdev-<session>` for direct SSH. For tmux-backed interactive sessions, force TTY (`ssh -tt okdev-<session>`).
- Use `okdev ssh --no-tmux` to bypass tmux for a single connection.
- Tmux uses a built-in okdev profile (history/mouse/vi-copy/status) and keeps the default command prefix (`Ctrl-b`).
- SSH keepalive can be tuned with:
  - `spec.ssh.keepAliveIntervalSeconds` (default `10`)
  - `spec.ssh.keepAliveTimeoutSeconds` (default `15`, must be `>= interval`)

## Preview Mode (No Cluster Mutations)

```bash
./bin/okdev up --dry-run
./bin/okdev sync --dry-run
./bin/okdev down --dry-run
```

## Multi-session Operations

```bash
./bin/okdev list
./bin/okdev use serving-main-alice
./bin/okdev status --all
# cross-owner visibility only when explicitly requested
./bin/okdev list --all-users
```

## Teardown and Cleanup

```bash
./bin/okdev down
./bin/okdev prune --ttl-hours 72
```
