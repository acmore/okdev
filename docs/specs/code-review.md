# Codebase Review: okdev

~30k lines of Go across 2 binaries and 12 internal packages. Overall the codebase is well-structured with good test coverage and clean package boundaries. Below are the findings organized by priority.

---

## P0 — Bugs & Correctness

No open P0 findings at the moment from this review pass.

---

## P1 — Performance

### 4. Repeated Kubernetes client creation (MOSTLY FIXED)
`internal/cli/common.go`, `internal/cli/ssh.go`, `internal/cli/connect.go`, `internal/cli/ports.go`, `internal/cli/sync.go`, `internal/cli/up.go`, `internal/cli/status.go`, `internal/cli/target.go`, `internal/cli/down.go`, and `internal/cli/prune.go` — a shared `commandContext` now creates and reuses one `kube.Client` per command in the main session-oriented flows. Some simpler commands still build their own client inline, but the repeated multi-step command paths no longer recreate the client several times during one execution.

### 5. ~~Shelling out to `ps` in a polling loop~~ (FIXED)
`processAlive()` now uses `os.FindProcess` + `Signal(0)` with an `isZombie()` guard that reads `/proc/<pid>/stat` on Linux (no subprocess) and falls back to `ps` only on macOS. `processLooksLikeSyncthingSync()` reads `/proc/<pid>/cmdline` on Linux with the same macOS fallback. The zombie check is necessary because `Signal(0)` succeeds for zombie processes on Unix.

### 6. ~~Marshal-unmarshal round-trip for type conversion~~ (FIXED)
`internal/workload/generic.go` now uses `runtime.DefaultUnstructuredConverter` for `PodTemplateSpec` conversion instead of a YAML marshal/unmarshal round-trip.

### 7. ~~`reflect.DeepEqual` on Kubernetes objects~~ (FIXED)
`internal/kube/client.go` now uses `apiequality.Semantic.DeepEqual` for Kubernetes API structs and `maps.Equal` for string metadata maps instead of raw `reflect.DeepEqual`.

### 8. ~~Bubble sort instead of `sort.Ints()`~~ (FIXED)
`internal/ssh/tunnel.go` now uses `sort.Ints()`.

---

## P2 — Architecture & Tech Debt

### 9. Massive function bodies (PARTIALLY FIXED)
- `internal/cli/ssh.go` — ~~`newSSHCmd()` RunE closure was 258 lines with reconnection logic inline.~~ Reconnection logic extracted to `runSSHShellWithReconnect()` with `sshShellRunner` interface, covered by 5 unit tests. The RunE closure is now ~70 lines shorter.
- `internal/cli/up.go` — `newUpCmd()` now delegates to `runUp()` plus explicit `upValidate()`, `upReconcile()`, `upDryRun()`, `upWait()`, and `upSetup()` phases. The UI flow is preserved, but there is still follow-up cleanup available inside setup helpers and some command helpers.

**Remaining work:** Continue trimming helper complexity in the SSH and up setup flows, especially around port-forward lifecycle and target refresh/setup sequencing.

### 10. ~~Unused parameters~~ (FIXED)
`startSessionMaintenance()` and `startSessionMaintenanceWithClient()` no longer accept `cfg` or `renewLock`. All 4 callers (ssh.go, connect.go, ports.go, sync.go) updated.

### 11. Duplicated config/namespace/session resolution (MOSTLY FIXED)
`internal/cli/common.go` now has `commandContext` + `resolveCommandContext()` and the repeated load-config → resolve-namespace → resolve-session → build-kube-client flow has been collapsed in `up`, `ssh`, `ssh-proxy`, `connect`, `ports`, `sync`, `status`, `target`, `down`, and `prune`.

**Remaining work:** Decide whether any remaining one-off helpers should adopt the same pattern, or whether the current scope is the right stopping point.

### 12. Hardcoded timeouts scattered across files (MOSTLY FIXED)
```
common.go:28    — 5 * time.Minute (heartbeat)
common.go:379   — 5 * time.Minute (context timeout)
ssh.go:114      — 10 * time.Second (keepalive)
ssh.go:231      — 5 * time.Second (rapid failure window)
ssh.go:241      — 60 * time.Second, 20 failures (circuit breaker)
syncthing.go:180 — 300ms sleep
portforward_helper.go:50 — 200ms ticker
```
These are now mostly centralized in `internal/cli/timeouts.go`, and the touched `common`, `ssh`, `sync`, `up`, `portforward_helper`, `workload`, `syncthing`, and proxy-health paths have been updated to use named constants.

**Remaining work:** Sweep the last few non-test literals and decide whether every remaining timing knob belongs in the shared timeout set or should stay local because it is formatting- or UX-specific.

### 13. ~~`goto` in migration code~~ (FIXED)
`internal/config/migrate.go` now uses a helper instead of `goto` to merge the workspace volume mount into the existing pod template.

### 14. ~~Inconsistent HTTP client patterns~~ (FIXED)
`internal/config/template.go` now uses an injectable HTTP doer and propagates context, aligning with the Syncthing installer pattern.

### 15. ~~No cache invalidation~~ (FIXED)
`internal/kube/client.go` now invalidates its client cache when kubeconfig inputs change, and `internal/workload/generic.go` now invalidates its cached manifest baseline when the manifest file changes on disk.

### 16. ~~Goroutine lifecycle management~~ (FIXED)
`internal/cli/common.go` now waits for the heartbeat goroutine to exit when session maintenance is stopped.

---

## P3 — Code Quality & Consistency

### 17. Inconsistent error handling strategy
Three patterns used interchangeably with no clear policy:
- Silent suppression: `_ = session.ClearActiveSession()` (common.go:254)
- Warning: `ui.warnf("failed to ...")` (down.go:72)
- Fatal: `return err`

Cleanup errors being silenced is fine, but should at least log at debug level for diagnosability.

### 18. Test interfaces in production code
`up.go:313`, `workload.go:69`, `portforward_helper.go:24` — Several narrow interfaces exist only for test doubles. Consider proper dependency injection at construction time instead.

### 19. Naming inconsistencies
- `resolveSessionName()` vs `resolveManagedSessionName()` vs `resolveSessionNameWithState()` — improved, but still not fully uniform
- No consistent parameter ordering for `(ctx, k, namespace, pod)` across functions
- `refreshTargetRef()` vs `loadTargetRef()` — unclear distinction

### 20. ~~Silent time-parse failures~~ (FIXED)
`internal/cli/prune.go` now logs a warning when `okdev.io/last-attach` cannot be parsed and falls back to `CreatedAt`.

### 21. ~~No response body size limit on syncthing downloads~~ (FIXED)
`internal/syncthing/installer.go` now applies explicit size limits to checksum and archive downloads.

---

## Summary

| Category | P0 | P1 | P2 | P3 |
|---|---|---|---|---|
| Bugs/Correctness | 0 | — | — | — |
| Performance | — | 2 | — | — |
| Architecture/Tech Debt | — | — | 4 | — |
| Code Quality | — | — | — | 3 |

## Top 3 Recommended Actions — Status

1. **Extract `newSSHCmd()` reconnection logic** — DONE. Extracted to `runSSHShellWithReconnect()` behind `sshShellRunner` interface, with 5 unit tests covering clean exit, non-recoverable error, successful recovery, context cancellation, and rapid failure circuit breaker.
2. **Remove unused params from `startSessionMaintenance`** — DONE. Removed `cfg` and `renewLock` from both `startSessionMaintenance()` and `startSessionMaintenanceWithClient()`, updated all 4 callers.
3. **Replace `ps`-based process polling with direct syscall** — DONE. `processAlive()` now uses `os.FindProcess` + signal(0). `processLooksLikeSyncthingSync()` reads `/proc/<pid>/cmdline` with macOS fallback to `ps`.

## Next Recommended Actions

1. **Trim helper complexity in SSH and up setup flows** — the top-level commands are much cleaner, but some setup helpers are still dense.
2. **Decide on the final scope of `commandContext` and `timeouts.go`** — most high-value call sites are migrated now, so the remaining work is cleanup and consistency rather than broad rollout.
3. **Address the remaining consistency items** — error-handling policy, naming cleanup, and whether test-only interfaces should be refactored further.
