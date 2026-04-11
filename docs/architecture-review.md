# okdev Architecture & Performance Review

**Date:** 2026-04-11
**Scope:** Full codebase (~100k lines Go)

## Project Overview

Go CLI for Kubernetes-based dev environments. Two binaries (`okdev`, `okdev-sshd`), clean `internal/` package structure with `cli`, `kube`, `workload`, `session`, `sync`, `ssh`, `connect`, `config` subsystems.

## Architecture Strengths

- **Workload abstraction** -- The `Runtime` interface with segregated client interfaces (`ApplyClient`, `DeleteClient`, etc.) follows ISP well. `GenericRuntime` supports arbitrary CRDs without per-type implementations.
- **Centralized timeouts** -- `timeouts.go` collects all timing constants, preventing silent divergence.
- **Consistent error wrapping** -- `fmt.Errorf("context: %w", err)` used throughout.
- **Interface-based DI** -- Port-forward helpers, sync health checkers, and kube operations accept interfaces for testability.
- **Kubernetes client caching** -- `cacheKey()` detects kubeconfig rotation by stat-ing files, avoiding stale clients.
- **SSH tunnel resilience** -- Exponential backoff, keepalive probes, rapid-failure detection, and state callbacks are thorough.

---

## Critical Findings

### C1. Goroutine leak in SSH keepalive timeout path

**File:** `internal/ssh/tunnel.go:518-538`

When keepalive times out, the goroutine calling `client.SendRequest` is never cancelled. It blocks indefinitely on the dead connection, leaking goroutines across reconnection cycles.

**Recommendation:** After detecting a timeout, close the underlying SSH client to unblock the goroutine. Confirm that `xssh.Client.Close()` reliably unblocks a concurrent `SendRequest` on all platforms.

### C2. Port-forward TOCTOU race in Syncthing API allocation

**File:** `internal/cli/syncthing.go:523-536`

The listener is closed before Syncthing binds to the port. Another process can steal it. The 3-attempt retry with log-sniffing for port conflicts is fragile.

**Recommendation:** Use port 0 in Syncthing's `--gui-address` and discover the actual port from Syncthing's API after startup, eliminating the race entirely.

### C3. Read-modify-write without file locking in `SaveInfo`

**File:** `internal/session/state.go:243-293`

`LoadInfo` -> merge -> `WriteFile` with no atomicity. Two terminals operating on the same session can corrupt state.

**Recommendation:** Use write-to-temp-then-rename for crash safety. Consider advisory file locking (`flock`) if concurrent multi-terminal usage is a supported scenario.

### C4. Unbounded deletion-wait loops

**File:** `internal/kube/client.go:324-360`

`waitForPodDeletion` and `waitForJobDeletion` poll at 300ms with no timeout. A finalizer that never completes hangs the CLI forever.

**Recommendation:** Add a default timeout (e.g., 5 minutes). Convert from polling to watches, consistent with `WaitReadyWithProgress` which already uses watches correctly.

### C5. `okdev-sshd` accepts all connections when authorized keys file is missing

**File:** `cmd/okdev-sshd/main.go:61-69`

When `loadAuthorizedKeys` returns nil, no `PublicKeyHandler` is set, so the server accepts unauthenticated connections. Defense-in-depth gap even though it runs inside a pod.

**Recommendation:** If no authorized keys are available, either refuse to start or set a handler that rejects all connections with a clear log message.

---

## Important Findings

### Performance

| ID | Location | Issue |
|----|----------|-------|
| I1 | `kube/client.go:144` | `cacheKey()` stats every kubeconfig file on every API call -- wasteful in hot loops (500ms polling) |
| I2 | `kube/client.go:170-286` | `Apply` deserializes YAML twice for typed resources (TypeMeta check then concrete type) |
| I3 | `workload/generic.go:211` | Double `DeepCopy` on cache miss in `load()` -- store original, copy only on return |
| I4 | `workload/helpers.go:183` | 500ms polling loop for candidate pod readiness -- should use a watch |
| I5 | `cli/sync.go:339-351` | `readFileTail` reads entire log file into memory before truncating -- use `Seek` |
| I6 | `cli/sync.go:550-562` | `isZombie` spawns a `ps` subprocess on every liveness check at 50ms intervals on macOS |

### Resource Leaks & Lifecycle

| ID | Location | Issue |
|----|----------|-------|
| I7 | `ssh/tunnel.go:83-85` | `runtime.SetFinalizer` for cleanup is unreliable -- finalizers may never run |
| I8 | `ssh/tunnel.go:260-275` | `WaitConnected` waiter channels accumulate on context cancellation -- never cleaned up |
| I9 | `cli/portforward_helper.go:25` | Port-forward goroutines use `context.Background()` -- disconnected from parent lifecycle, missed `cancelAndWait` leaks the goroutine + SPDY connection |
| I10 | `ssh/tunnel.go:199-249` | `ListListeningPorts` in auto-detect ignores the cancel context -- old goroutines linger up to 10s |
| I11 | `cli/ssh.go:450` | `ensureDevSSHDRunning` called with `context.Background()`, ignoring the caller's timeout |

### Architecture & Design

| ID | Location | Issue |
|----|----------|-------|
| I12 | `kube/podspec.go:93-94` | Sidecar runs as fully `Privileged` -- should use scoped capabilities if possible |
| I13 | `kube/podspec.go:17` | `PreparePodSpecForTarget` has 9 positional parameters -- options struct would improve safety |
| I14 | `workload/pod.go` + `generic.go` | Duplicated sidecar config fields across both runtimes -- extract shared `SidecarConfig` struct |
| I15 | `session/state.go:48-64` | `RepoRoot` uses process-global `sync.Once` -- breaks tests (already hacked around) and prevents daemon mode |
| I16 | `cli/common.go:123-134` | `withQuietConfigAnnounce` mutates `os.Environ` -- not concurrency-safe with background goroutines |
| I17 | `kube/client.go:320` | `applyUnstructured` uses full `Update` instead of server-side Apply `Patch` -- conflicts with controllers |

---

## Minor Findings

| ID | Location | Issue |
|----|----------|-------|
| M1 | `cli/portforward_helper.go:71-107` | Two port-forward functions share ~70% code -- extract common goroutine+cancel logic |
| M2 | `ssh/tunnel.go:319-321` | `Exec` wraps `ExecContext` with `context.Background()` -- no timeout protection |
| M3 | `connect/runner.go:79-103` | `isRetryableError` uses string matching on error messages -- fragile, prefer `errors.Is`/`errors.As` |
| M4 | `cli/up.go:567-680` | `refreshTargetRef` called 5+ times in `upSetup` -- each is a K8s API round-trip |
| M5 | `cli/proxy_health.go:81-87` | Mixed `logx.Printf` + `slog.Debug` creates duplicate log entries with divergent formats |
| M6 | `cli/lifecycle.go:11-54` | Three identical lifecycle hook resolvers -- extract common `resolveLifecycleCommand` helper |
| M7 | `kube/client.go:1074` | `DescribePod` event listing is unbounded with discarded error |
| M8 | `session/state.go:243-293` | `SaveInfo` merge can never clear a field back to empty |
| M9 | `kube/client.go:1038` | `tempDownloadPath` has TOCTOU race (creates temp, removes it, returns path) |

---

## Recommended Priority

### Fix first (correctness/reliability)

1. **C3** -- Atomic state writes (write-temp-rename), protects against data corruption
2. **C4** -- Add timeout to deletion-wait loops, convert to watches
3. **C1** -- Fix keepalive goroutine leak on timeout
4. **C5** -- Reject unauthenticated SSH when no keys configured

### Fix second (performance/resource management)

5. **I6** -- Stop spawning `ps` at 50ms intervals
6. **I9/I11** -- Propagate parent context to port-forwards and SSHD startup
7. **I8** -- Clean up orphaned waiter channels
8. **I1** -- Add short TTL cache for kubeconfig stats

### Improve later (design quality)

9. **I13/I14** -- Options struct and shared sidecar config
10. **I15/I16** -- Eliminate global mutable state (`sync.Once`, `os.Setenv`)
