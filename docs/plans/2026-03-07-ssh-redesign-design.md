# SSH Redesign Design

## Problem Statement

The current okdev SSH implementation has several structural weaknesses:

1. **Connection drops** after a few minutes with no auto-reconnect
2. **SSH lands in the sidecar** instead of the dev container
3. **No auto-detect port forwarding** — only configured ports are forwarded
4. **Process management complexity** — shelling out to `ssh` binary, orphan master processes, stale control sockets
5. **Two separate sidecars** (syncthing + okdev-ssh) add pod complexity

## Design

### 1. Merged Sidecar

Replace the separate `syncthing` and `okdev-ssh` sidecar containers with a single `okdev-sidecar` container.

```
┌─────────────────────────────────────────────┐
│ Pod: okdev-<session>                        │
│                                             │
│  ┌──────────────┐  ┌────────────────────┐   │
│  │ dev container │  │ okdev-sidecar      │   │
│  │ (user image)  │  │ - sshd (:22)       │   │
│  │              │  │ - syncthing (:8384) │   │
│  │ /workspace ──┼──┼── /workspace        │   │
│  └──────────────┘  └────────────────────┘   │
└─────────────────────────────────────────────┘
```

- Image: `ghcr.io/acmore/okdev:<version>` (same image as today's syncthing sidecar, gains sshd)
- Entrypoint runs a simple process manager to start both sshd + syncthing
- Pod container count reduced from 3 to 2
- SSH `mode` config field removed — sidecar is always used

### 2. SSH into Dev Container via nsenter

The sidecar runs sshd, but the shell session lands in the dev container using shared PID namespace + nsenter.

- Pod spec sets `shareProcessNamespace: true`
- sshd `ForceCommand` uses `nsenter --target <pid> --mount --uts --ipc --pid -- /bin/sh -l`
- The sidecar finds the dev container's PID 1 by scanning `/proc`
- No kubectl binary or RBAC needed in the sidecar

**Flow:**
1. User runs `okdev ssh` or `ssh okdev-<session>`
2. Connects to sshd on port 22 in the sidecar
3. sshd authenticates with ed25519 key
4. `ForceCommand` runs nsenter into dev container's namespaces
5. User gets a shell inside the dev container

### 3. In-Process Go SSH Tunnel Manager

New package `internal/ssh` with a `TunnelManager` that replaces shelling out to the `ssh` binary.

Uses `golang.org/x/crypto/ssh` for full control over connection lifecycle.

```
┌─────────────────────────────────────────────────────┐
│ TunnelManager                                       │
│                                                     │
│  kubectl port-forward ──► sshd (:22)                │
│       ↕                                             │
│  Go x/crypto/ssh client                             │
│       ↕                                             │
│  ┌─────────────┐ ┌──────────────┐ ┌──────────────┐  │
│  │ Local Fwds  │ │ Auto-Detect  │ │ Shell        │  │
│  │ spec.ports  │ │ port scanner │ │ session      │  │
│  └─────────────┘ └──────────────┘ └──────────────┘  │
│                                                     │
│  Reconnect loop (exponential backoff)               │
└─────────────────────────────────────────────────────┘
```

**Core type:**

```go
type TunnelManager struct {
    namespace    string
    pod          string
    sshUser      string
    sshKeyPath   string
    remotePort   int

    client       *ssh.Client
    forwards     []Forward        // configured spec.ports
    autoForwards []Forward        // auto-detected
    mu           sync.Mutex
    ctx          context.Context
    cancel       context.CancelFunc
}
```

**Key methods:**

| Method | Purpose |
|--------|---------|
| `Connect(ctx)` | Establish kubectl port-forward + SSH connection |
| `Reconnect()` | Teardown + re-establish with backoff (1s, 2s, 4s... max 30s) |
| `AddForward(local, remote)` | Add a local port forward as SSH channel |
| `RemoveForward(local)` | Remove a forward |
| `StartAutoDetect(interval)` | Poll `ss -tlnp` in dev container, diff, auto-add/remove |
| `OpenShell()` | Interactive shell session (for `okdev ssh`) |
| `Close()` | Teardown everything |

**Reconnect loop:**
- Background goroutine after `Connect()`
- Monitors SSH connection health via keepalive (every 10s)
- On failure: teardown, exponential backoff (1s, 2s, 4s... max 30s), re-establish port-forward + SSH + all forwards
- Logs: `"Connection lost, reconnecting..."` / `"Reconnected successfully"`

### 4. Port Forwarding — Configured + Auto-Detect

**Configured forwards** from `spec.ports`:

```yaml
spec:
  ports:
    - name: api
      local: 8080
      remote: 8080
    - name: debug
      local: 9229
      remote: 9229
```

Established immediately on connect and re-established on reconnect. Implemented as SSH channels instead of separate kubectl port-forwards.

**Auto-detect forwards:**

- Goroutine runs every 2 seconds
- Executes `ss -tlnp` (fallback: `cat /proc/net/tcp`) in the dev container via SSH exec
- Parses output, diffs against known forwards
- New listener: auto-forward with `local=remote`
- Listener gone: remove auto-forward
- Skips conflicts with configured forwards and excluded ports

**Exclusion list** (never auto-forward):

| Port | Reason |
|------|--------|
| 22 | sshd |
| 8384 | syncthing GUI |
| 22000 | syncthing sync |

**User-facing output:**

```
Auto-forwarded port 3000 (localhost:3000 → pod:3000)
Auto-forwarded port 5432 (localhost:5432 → pod:5432)
Port 3000 no longer listening, removed forward
```

**Config option:**

```yaml
spec:
  ssh:
    autoDetectPorts: true  # default true
```

### 5. SSH Config Compatibility

`~/.ssh/config` entries are still written so `ssh okdev-<session>` works from any terminal.

```
# BEGIN OKDEV okdev-myapp
Host okdev-myapp
  HostName 127.0.0.1
  User root
  IdentityFile ~/.okdev/ssh/id_ed25519
  StrictHostKeyChecking no
  UserKnownHostsFile /dev/null
  ProxyCommand okdev --session 'myapp' ssh-proxy
  ServerAliveInterval 30
  ServerAliveCountMax 10
# END OKDEV okdev-myapp
```

- No `LocalForward` lines — forwards managed by TunnelManager
- `okdev ssh-proxy` rewritten to use Go SSH internally
- Lifecycle: `okdev up` writes entry, `okdev down` removes it

### 6. Command Changes

**`okdev up`:**
1. Create pod with merged `okdev-sidecar` container
2. Wait for pod ready
3. Install SSH key in sidecar
4. Write `~/.ssh/config` entry
5. Start TunnelManager as background process (PID file at `~/.okdev/ssh/okdev-<session>.pid`)
6. Start syncthing sync

**`okdev ssh`:**
- Creates TunnelManager, connects, opens interactive shell
- Shell lands in dev container via nsenter
- On disconnect: reconnect if dropped, exit if user typed `exit`

**`okdev down`:**
- Stop background TunnelManager via PID file
- Remove `~/.ssh/config` entry
- Delete pod

**`okdev ssh-proxy`:**
- Rewritten with Go SSH (kubectl port-forward + SSH + stdin/stdout bridge)

### 7. Config Changes

**Removed fields:**
- `spec.ssh.localPort` — no longer needed (Go SSH manages port binding)
- `spec.ssh.mode` — sidecar is always used
- `spec.ssh.sidecarImage` — replaced by sidecar.image

**New fields:**
- `spec.ssh.autoDetectPorts` (bool, default true)
- `spec.sidecar.image` (string, default `ghcr.io/acmore/okdev:<version>`)

**Kept fields:**
- `spec.ssh.user` (default "root")
- `spec.ssh.remotePort` (default 22)
- `spec.ssh.privateKeyPath`

### 8. Code Removed

- `startManagedSSHForward()` / `stopManagedSSHForward()` — no OpenSSH master
- `sshControlSocketPath()` — no control sockets
- `reserveEphemeralPort()` — Go SSH handles port binding
- `startSSHPortForwardWithFallback()` — replaced by TunnelManager
- `isPortListenConflict()` — no longer needed
- `sshTargetContainer()` — sidecar is always used

### 9. New Dependencies

- `golang.org/x/crypto/ssh` — Go SSH client library
