# SSH Proxy Health Monitoring and Keep-Alive

## Problem

When `okdev ssh-proxy` (the ProxyCommand used by external `ssh` clients) becomes unresponsive, the user's terminal freezes with no way to recover. The proxy's `io.Copy` loops block indefinitely, there is no signal handling, and the port-forward keepalive's `onDegraded` callback is `nil` — so degradation is detected but never acted on.

## Design Principles

- Each network layer owns its own health monitoring — no layer depends on another to detect failures.
- Fail fast, exit clean — the proxy cannot reconnect an SSH session, so the best UX is getting the user their terminal back quickly.
- Silent to the user, verbose in logs — no stderr output; detailed diagnostics go to the okdev log file.

## Proxy Health Monitoring (Three Layers)

### Layer 1: TCP Keepalive

Set via socket options on the data connection returned by `waitDialLocal()`.

- Interval: 5 seconds
- Probe count: 2
- Detects: dead peer, network partition, crashed port-forward process
- Detection time: ~15 seconds

### Layer 2: Data Flow Monitor

A wrapper around `io.Copy` that tracks data flow timestamps.

- `monitoredCopy` updates an atomic `lastDataTime` on every successful read/write
- A watchdog goroutine checks `lastDataTime` every 5 seconds
- If no data flows in either direction for 15 seconds, close the connection and exit
- Detects: application-level stalls, hung connections where TCP stays alive
- Detection time: 15-20 seconds

### Layer 3: Port-Forward Keepalive Probe

The existing `startPortForwardKeepalive` probe, with the `onDegraded` callback wired up.

- Interval: 10 seconds (existing)
- Failure threshold: 2 consecutive probe failures (reduced from 3)
- `onDegraded` callback closes the data connection, triggering exit
- Detects: kubectl port-forward process death, SPDY/WebSocket channel stall
- Detection time: ~20 seconds

### Combined Detection

Any single layer detecting a problem triggers exit. Worst-case detection time: ~20 seconds.

## Exit Behavior

On detection of any failure:

1. Log detailed diagnostics to okdev log file (which layer detected, error details, connection duration, timestamps)
2. Close the data connection — unblocks both `io.Copy` goroutines
3. Exit with non-zero exit code (exit 1)
4. No output to stderr — the `ssh` client reports the disconnect naturally

No reconnection attempt. Re-establishing the port-forward cannot save the in-flight SSH session because SSH protocol state (encryption keys, sequence numbers, channel state) is tied to the original TCP stream.

### Signal Handling

Register SIGINT/SIGTERM handler for clean shutdown: close connections, cancel port-forward keepalive, stop kubectl port-forward process. Prevents orphaned goroutines and processes.

## SSH Client Config Changes

The okdev-managed `Host` block in `~/.ssh/config` gets tighter keepalive values:

| Setting | Old | New |
|---------|-----|-----|
| `ServerAliveInterval` | 10 | 5 |
| `ServerAliveCountMax` | 30 | 10 |
| `TCPKeepAlive` | yes | yes (unchanged) |

This gives the SSH client its own independent detection at ~55 seconds. The proxy-side detection (~15-20s) is the fast path; the SSH client config is a fallback safety net.

## Implementation Scope

### Files to Modify

**`internal/cli/ssh.go` — `newSSHProxyCmd`:**
- Set TCP keepalive options on the data connection (5s interval, 2 probes)
- Replace raw `io.Copy` with `monitoredCopy` that tracks data flow timestamps
- Add watchdog goroutine (5s check interval, 15s idle threshold)
- Wire `onDegraded` callback in `startPortForwardKeepalive` to close connection
- Add signal handler (SIGINT/SIGTERM) for clean shutdown
- Add structured logging on exit (layer that detected, duration, error)

**`internal/cli/ssh.go` — SSH config writing:**
- Change `ServerAliveInterval` to 5
- Change `ServerAliveCountMax` to 10

**`internal/cli/common.go` or new file — `monitoredCopy` helper:**
- Wraps `io.Copy` with atomic timestamp updates
- Small, focused utility

### Files NOT Modified

- `tunnel.go` — the proxy does not use the tunnel manager
- `config.go` — no new config fields; values are hardcoded for the proxy path
- `portforward_helper.go` — existing probe logic is sufficient

## Testing

### Unit Tests

- `monitoredCopy`: verify timestamp updates on data flow, verify idle detection triggers close after threshold
- `onDegraded` callback wiring: verify connection closure on port-forward degradation

### Integration Tests (Manual)

- Kill kubectl port-forward mid-session: verify proxy exits within ~20 seconds
- Suspend the pod (simulate network stall): verify data flow monitor catches it
- Normal idle session with SSH keepalives: verify no false positive disconnects
