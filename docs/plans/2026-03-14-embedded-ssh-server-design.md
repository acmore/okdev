# Embedded SSH Server Design

## Problem

The current sidecar SSH architecture uses `nsenter` to cross container boundaries
on every SSH session. This causes:

- Processes run in the sidecar's cgroup, hitting the sidecar's memory/CPU limits
  instead of the dev container's (e.g., OOM during flash-attn builds)
- tmux session persistence is fragile (sshd kills tmux server on disconnect)
- Extra complexity from the nsenter wrapper (PID discovery, namespace flags)

## Solution

Add an embedded SSH server mode (`--ssh-mode embedded`) that runs a lightweight
Go SSH binary directly inside the dev container. This matches okteto's approach
with `okteto/remote`.

## Architecture

### Sidecar mode (current, default)

```
ssh client → ProxyCommand → port-forward :22 → sidecar sshd → nsenter → dev shell
```

### Embedded mode (new)

```
ssh client → ProxyCommand → port-forward :2222 → okdev-sshd (in dev container) → dev shell
```

The sidecar still runs (hosts syncthing, injects the binary). SSH sessions go
directly to the embedded server — no nsenter per session.

## Components

### 1. `okdev-sshd` binary (`cmd/okdev-sshd/main.go`)

- Built with `gliderlabs/ssh`, static binary (`CGO_ENABLED=0`)
- Features: PTY sessions, exec commands, SFTP, public key auth
- Flags: `--port`, `--authorized-keys`, `--shell`
- Auto-detects shell (`/bin/bash` → `/bin/sh` fallback)

### 2. Sidecar image (`infra/sidecar/Dockerfile`)

- Multi-stage build: Go build stage → copy `okdev-sshd` into Alpine image
- Binary placed at `/usr/local/bin/okdev-sshd`

### 3. Entrypoint changes (`infra/sidecar/entrypoint.sh`)

- New env var: `OKDEV_SSH_MODE` ("embedded" or "sidecar")
- When embedded:
  - Copy `okdev-sshd` binary into dev container via `nsenter --mount`
  - Copy `authorized_keys` into dev container
  - Start `okdev-sshd` inside dev container via `nsenter --mount --uts --ipc --pid --cgroup`
- sshd still starts for syncthing and backward compat

### 4. CLI changes (`internal/cli/up.go`)

- `--ssh-mode` flag: `"sidecar"` (default) or `"embedded"`
- Pass `OKDEV_SSH_MODE` env var to sidecar container
- Port-forward to 2222 instead of 22 when embedded

### 5. Pod spec changes (`internal/kube/podspec.go`)

- Add port 2222 to sidecar container spec when embedded mode
- Pass `OKDEV_SSH_MODE` env var

## Key Differences from Sidecar Mode

| Aspect | Sidecar | Embedded |
|--------|---------|----------|
| SSH server location | sidecar container | dev container |
| cgroup | sidecar limits | dev container limits |
| Session persistence | requires tmux | not needed (native process) |
| nsenter per session | yes | no (only at startup to inject binary) |
| Port | 22 | 2222 |

## What Stays the Same

- ProxyCommand (`okdev ssh-proxy`)
- SSH config entry in `~/.ssh/config`
- Port-forward keepalive mechanism
- Syncthing (still in sidecar)
- `okdev ssh` reconnect logic

## Error Handling

- If `okdev-sshd` fails to start, fall back to sidecar mode with warning
- Binary not found in sidecar image → error at `okdev up`, suggest upgrade

## Scope

### In scope

- `okdev-sshd` Go binary with PTY, exec, public key auth, SFTP
- `--ssh-mode embedded` flag on `okdev up`
- Sidecar entrypoint logic to inject and start the binary
- Port-forward routing to embedded server

### Out of scope (future)

- Removing sidecar sshd entirely
- Reverse port forwarding in `okdev-sshd`
- Making embedded the default
- Config-level `ssh.mode` setting
