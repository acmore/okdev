# SSH Proxy Health Monitoring and Keep-Alive

## Problem

When `okdev ssh-proxy` (the ProxyCommand used by external `ssh` clients) becomes unresponsive, the user's terminal freezes with no way to recover. The proxy's `io.Copy` loops block indefinitely, there is no signal handling, and the port-forward keepalive's `onDegraded` callback is `nil` — so degradation is detected but never acted on.

## Design Principles

- Each network layer owns its own health monitoring — no layer depends on another to detect failures.
- Fail fast, exit clean — the proxy cannot reconnect an SSH session, so the best UX is getting the user their terminal back quickly.
- Silent to the user, verbose in logs — no stderr output; detailed diagnostics go to the okdev log file.

## Proxy Health Monitoring (Three Layers)

### Layer 1: TCP Keepalive

Modify the existing TCP keepalive in `waitDialLocal()` (currently 10s period via `SetKeepAlivePeriod`).

- Reduce keepalive period from 10s to 5s via `SetKeepAlivePeriod(5 * time.Second)`
- Optionally set `TCP_KEEPCNT` to 2 via platform-specific syscall (`syscall.SetsockoptInt` on the raw fd) when supported. This is an optimization, not a correctness dependency; if unavailable or unsupported on a target platform, log at debug level and continue with the standard Go keepalive settings. Go 1.24+ sets `TCP_KEEPIDLE` and `TCP_KEEPINTVL` via `SetKeepAlivePeriod`.
- Detects: local loopback socket failure between `ssh-proxy` and the local `kubectl port-forward` process, including crashed port-forward process or dead local peer
- Does not independently detect: remote cluster-side network partitions or pod-side stalls, because this socket is `127.0.0.1:<localPort>`
- Detection time: implementation-dependent by OS; with `SetKeepAlivePeriod(5s)` and `TCP_KEEPCNT=2` where supported, expected detection is roughly ~15 seconds

### Layer 2: Data Flow Monitor

A wrapper around `io.Copy` that tracks data flow timestamps.

- `monitoredCopy` updates an atomic `lastDataTime` on every successful read/write
- A watchdog goroutine checks `lastDataTime` every 5 seconds
- If no data flows in either direction for 15 seconds, close the connection and exit
- The watchdog only triggers when both copy directions are still active (neither has exited normally). A normal stdin EOF from the SSH client disconnecting is not a failure.
- Detects: application-level stalls, hung connections where TCP stays alive
- Detection time: 15-20 seconds
- Note: SSH keepalive packets (`ServerAliveInterval`) flow through the proxied TCP stream, so `monitoredCopy` sees them as data. With `ServerAliveInterval 5`, traffic flows at least every 5s, preventing false idle detection on legitimately quiet sessions.

### Layer 3: Port-Forward Keepalive Probe

The existing `startPortForwardKeepalive` probe, with the `onDegraded` callback wired up.

- Interval: 10 seconds (existing)
- Failure threshold: 3 consecutive probe failures (existing, unchanged — changing this would require modifying `portforward_helper.go` which is out of scope)
- `onDegraded` callback closes the data connection, triggering exit
- Sequencing constraint: `startPortForwardKeepalive` is currently called before `waitDialLocal` creates `conn`. Move the keepalive start to after the dial, or use an atomic pointer to `conn` so the callback can reference it once set.
- Detects: kubectl port-forward process death, SPDY/WebSocket channel stall
- Detection time: ~30 seconds

### Combined Detection

Any single layer detecting a problem triggers exit. Multiple layers may detect failure simultaneously; concurrent `conn.Close()` calls are safe because the existing code uses a `sync.Once` for error capture and the `done` channel pattern for completion signaling. Worst-case detection time: ~30 seconds.

## Exit Behavior

On detection of any failure:

1. Log detailed diagnostics to okdev log file (which layer detected, error details, connection duration, timestamps)
2. Close the data connection — unblocks both `io.Copy` goroutines
3. Return a proxy-specific sentinel error from `RunE`
4. Suppress that sentinel error in `cmd/okdev/main.go` so the process still exits non-zero without writing to stderr
5. No output to stderr for expected proxy health-triggered disconnects — the `ssh` client reports the disconnect naturally

No reconnection attempt. Re-establishing the port-forward cannot save the in-flight SSH session because SSH protocol state (encryption keys, sequence numbers, channel state) is tied to the original TCP stream.

### Signal Handling

Register SIGINT/SIGTERM handler for clean shutdown:
- Close the data connection
- Cancel `pfKeepAliveCtx` (stops the port-forward keepalive goroutine)
- Cancel port-forward via `cancelPF()` (stops the kubectl port-forward process)

Use `cmd.Context()` instead of `context.Background()` for the keepalive context so cobra's built-in signal handling integrates cleanly. The signal handler and health watchdog both converge on the same action: close the connection and exit.

## SSH Client Config Changes

The okdev-managed `Host` block in `~/.ssh/config` gets tighter keepalive values. These are hardcoded in the SSH config writing code in `ssh.go`, independent of `config.go` defaults (which remain unchanged for the `okdev ssh` direct path).

| Setting | Old | New |
|---------|-----|-----|
| `ServerAliveInterval` | 10 (from config default) | 5 (hardcoded for proxy path) |
| `ServerAliveCountMax` | 30 (from config default) | 10 (hardcoded for proxy path) |
| `TCPKeepAlive` | yes | yes (unchanged) |

This gives the SSH client its own independent detection at ~55 seconds. The proxy-side detection (~15-30s) is the fast path; the SSH client config is a fallback safety net.

## Implementation Scope

### Files to Modify

**`internal/cli/ssh.go` — `newSSHProxyCmd`:**
- Reduce TCP keepalive period to 5s and optionally set `TCP_KEEPCNT=2` via syscall on the data connection where supported
- Replace raw `io.Copy` with `monitoredCopy` that tracks data flow timestamps
- Add watchdog goroutine (5s check interval, 15s idle threshold)
- Wire `onDegraded` callback in `startPortForwardKeepalive` to close connection
- Use `cmd.Context()` instead of `context.Background()` for keepalive context
- Add structured logging on exit (layer that detected, duration, error)

**`cmd/okdev/main.go`:**
- Suppress the proxy sentinel error from stderr while still exiting with status 1

**`internal/cli/ssh.go` — SSH config writing:**
- Hardcode `ServerAliveInterval 5` and `ServerAliveCountMax 10` for the proxy path

**`internal/cli/common.go` or new file — `monitoredCopy` helper:**
- Wraps `io.Copy` with atomic timestamp updates
- Small, focused utility

### Files NOT Modified

- `tunnel.go` — the proxy does not use the tunnel manager
- `config.go` — no new config fields; proxy values are hardcoded separately
- `portforward_helper.go` — existing probe logic and thresholds are sufficient

## Testing

### Unit Tests

- `monitoredCopy`: verify timestamp updates on data flow, verify idle detection triggers close after threshold
- `onDegraded` callback wiring: verify connection closure on port-forward degradation

### Integration Tests (Manual)

- Kill kubectl port-forward mid-session: verify proxy exits within ~30 seconds
- Suspend the pod (simulate network stall): verify data flow monitor catches it
- Normal idle session with SSH keepalives: verify no false positive disconnects
