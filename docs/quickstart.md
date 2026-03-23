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

## Configuration Discovery

1. `-c, --config <path>`
2. `.okdev.yaml`
3. `okdev.yaml`

## Kubernetes Context

1. `--context`
2. `spec.kubeContext` in manifest
3. active kube context from your kubeconfig

## Start or Resume Session

```bash
./bin/okdev up
```

### Named Sessions

```bash
./bin/okdev up --session serving-main-alice
```

### Disable Tmux

```bash
./bin/okdev up --no-tmux
```

### What `okdev up` Does

- ensures Pod/PVC state
- writes managed `~/.ssh/config` host entry (`okdev-<session>`)
- starts managed SSH/forwarding bootstrap
- starts background Syncthing sync in bidirectional mode when enabled
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

## Notes

- Local Syncthing is auto-installed when `sync.engine=syncthing`.
- `okdev up` starts background sync in bidirectional mode by default.
- Default sidecar image (syncthing + okdev-sshd binary) follows binary version: `ghcr.io/<owner>/okdev:<okdev-version>` (`edge` for dev builds).
- `spec.sync.exclude` applies local ignore patterns; `spec.sync.remoteExclude` applies remote-only ignores.
- SSH target is the `dev` container via `okdev-sshd` on port `2222`.
- `spec.ports` are rendered as SSH `LocalForward` entries.
- Local runtime state/logs are stored under `~/.okdev/` (not in the project working directory).
- Use `ssh okdev-<session>` for direct SSH. For tmux-backed interactive sessions, force TTY (`ssh -tt okdev-<session>`).
- Use `okdev ssh --no-tmux` to bypass tmux for a single connection.
- Use `spec.ssh.persistentSession: false` or `okdev up --no-tmux` if you want non-tmux dev-container sessions by default.
- The managed SSH host entry uses tighter proxy keepalive settings than the general CLI defaults so hung `ssh okdev-<session>` sessions fail fast instead of freezing indefinitely.
- Proxy health diagnostics are written to `~/.okdev/logs/okdev.log`, not printed into the SSH client session.
- Tmux uses a built-in okdev profile (history/mouse/vi-copy/status) and keeps the default command prefix (`Ctrl-b`).
- SSH keepalive can be tuned with `spec.ssh.keepAliveIntervalSeconds` (default `30`) and `spec.ssh.keepAliveTimeoutSeconds` (default `90`, must be `>= interval`).

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

## Advanced Recovery Commands

```bash
# Reconcile managed SSH LocalForward rules from spec.ports
./bin/okdev ports

# Run sync in foreground for troubleshooting or one-way flows
./bin/okdev sync --mode up
./bin/okdev sync --mode down
```

## Teardown and Cleanup

```bash
./bin/okdev down
./bin/okdev prune --ttl-hours 72
```
