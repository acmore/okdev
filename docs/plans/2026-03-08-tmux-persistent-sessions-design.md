# Tmux Persistent Shell Sessions

## Problem

SSH sessions die on network drops (kube port-forward instability, WiFi blips). The SSH protocol cannot resume a session on a new TCP connection. Users lose their shell state and must restart their work.

## Solution

Wrap interactive SSH sessions in tmux on the server side. On reconnect, users automatically reattach to their existing shell session. Controlled by config and CLI flag, off by default.

## Design

### Activation

- Config field: `spec.ssh.persistentSession` (bool pointer, default false)
- CLI flag: `okdev up --tmux` (overrides config)
- Per-connection opt-out: `okdev ssh --no-tmux` (sends `OKDEV_NO_TMUX=1` env var via SSH)

### Implementation

The change is server-side in the sidecar's `nsenter-dev.sh` ForceCommand handler. No changes to Go SSH client code.

1. `okdev up --tmux` sets env var `OKDEV_TMUX=1` on the sidecar container in the pod manifest
2. `nsenter-dev.sh` checks `OKDEV_TMUX=1`, `OKDEV_NO_TMUX!=1`, and whether the session has a TTY
3. If all conditions met: `exec nsenter ... tmux new-session -A -s okdev`
4. Otherwise: bare shell or command as today

### Interactive vs Non-Interactive Detection

```bash
if [ "$OKDEV_TMUX" = "1" ] && [ "$OKDEV_NO_TMUX" != "1" ] && [ -t 0 ]; then
    exec nsenter ... tmux new-session -A -s okdev
else
    exec nsenter ... "$@"
fi
```

### Session Naming

Single `okdev` tmux session with multiple windows. `tmux new-session -A -s okdev` creates if missing, attaches if exists. New connections become new windows in the same session.

### tmux Availability

tmux must be installed in the sidecar image. If missing at runtime, fall back to bare shell with a warning to stderr.

### Toggling

Requires pod restart. `okdev up --tmux` enables, `okdev up` (without flag) disables. Consistent with other pod-level settings.

## Components Modified

- `internal/config/config.go` — add `PersistentSession *bool` to `SSHSpec`, wire defaults
- `internal/cli/up.go` — add `--tmux` flag, merge with config value
- Pod builder (wherever sidecar env vars are set) — set `OKDEV_TMUX=1` on sidecar container
- `internal/cli/ssh.go` — add `--no-tmux` flag, send `OKDEV_NO_TMUX=1` env var
- `infra/sidecar/entrypoint.sh` / `nsenter-dev.sh` — check env vars + TTY, wrap with tmux
- Sidecar Dockerfile — install tmux

## Out of Scope

- Auto-reconnect for port forwards (separate feature)
- Client-side changes to `OpenShell()`
- Multi-user session isolation
