# Codebase Review: okdev

~30k lines of Go across 2 binaries and 12 internal packages. Overall the codebase is well-structured with good test coverage and clean package boundaries. Below are the findings organized by priority.

---

## P0 — Bugs & Correctness

No open P0 findings at the moment from this review pass.

---

## P1 — Performance

### 4. ~~Repeated Kubernetes client creation~~ (FIXED)
`internal/cli/common.go`, `internal/cli/ssh.go`, `internal/cli/connect.go`, `internal/cli/ports.go`, `internal/cli/sync.go`, `internal/cli/up.go`, `internal/cli/status.go`, `internal/cli/target.go`, `internal/cli/down.go`, and `internal/cli/prune.go` — a shared `commandContext` now creates and reuses one `kube.Client` per command in the main session-oriented flows. `list.go` removed its duplicate client creation. `runSyncthingSync` now receives the kube client from its caller instead of creating a new one. Remaining standalone commands (`init`, `validate`, `version`, `use`, `migrate`) don't follow the session pattern and correctly build their own clients.

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

### 9. ~~Massive function bodies~~ (FIXED)
- `internal/cli/ssh.go` — Reconnection logic extracted to `runSSHShellWithReconnect()` with `sshShellRunner` interface, covered by 5 unit tests. The RunE closure is now ~70 lines shorter.
- `internal/cli/up.go` — `newUpCmd()` now delegates to `runUp()` plus explicit `upValidate()`, `upReconcile()`, `upDryRun()`, `upWait()`, and `upSetup()` phases.

### 10. ~~Unused parameters~~ (FIXED)
`startSessionMaintenance()` and `startSessionMaintenanceWithClient()` no longer accept `cfg` or `renewLock`. All 4 callers (ssh.go, connect.go, ports.go, sync.go) updated.

### 11. ~~Duplicated config/namespace/session resolution~~ (FIXED)
`internal/cli/common.go` now has `commandContext` + `resolveCommandContext()` and the repeated load-config → resolve-namespace → resolve-session → build-kube-client flow has been collapsed in `up`, `ssh`, `ssh-proxy`, `connect`, `ports`, `sync`, `status`, `target`, `down`, and `prune`. Remaining standalone commands (`list`, `init`, `validate`, `version`, `use`, `migrate`) don't follow the session pattern and correctly handle their own setup.

### 12. ~~Hardcoded timeouts scattered across files~~ (FIXED)
All operational timeouts are now centralized in `internal/cli/timeouts.go`. The two remaining inline literals (`24*time.Hour` in `formatAge()` and `10*time.Millisecond` in RTT display rounding) are display/formatting constants, not operational timeouts, and are appropriately kept local.

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

### 17. ~~Inconsistent error handling strategy~~ (FIXED)
Previously suppressed errors now log at appropriate levels:
- `stopManagedSSHForward` failures in `down.go`, `ports.go`, `up.go` → `slog.Debug`
- PVC deletion failure in `prune.go` → `slog.Warn`
- Process lifecycle operations in `sync.go` (PID file removal, log file close, signal sending) → `slog.Debug`
- Syncthing process operations in `syncthing.go` (log file close, process release) → `slog.Debug`
- TCP socket tuning, connection close in defer paths, and environment variable operations remain silently suppressed as these are best-effort and expected to succeed.

### 18. ~~Test interfaces in production code~~ (FIXED — by design)
The narrow interfaces (`workloadExistenceChecker`, `targetResolverClient`, `sshShellRunner`, etc.) follow Go's interface segregation principle: accept the narrowest interface that satisfies the function's needs. This is idiomatic Go, not a test-only pattern. Each interface is used at the function boundary for both production dispatch (via `*kube.Client`) and test doubles, which is the intended Go approach. No refactoring needed.

### 19. ~~Naming inconsistencies~~ (FIXED)
- `resolveManagedSessionName()` and `resolveSessionNameForUpDown()` were identical to `resolveSessionName()` — both removed. All callers now use `resolveSessionName()`.
- `loadTargetRef()` / `resolveTargetRef()` / `refreshTargetRef()` follow a clear progression: load=disk, resolve=cluster+disk, refresh=validate+resolve. Names are intentionally distinct.
- Parameter ordering: functions accepting `(ctx, k, namespace, ...)` vs `(ctx, opts, cfg, namespace, ..., k)` reflects two patterns — utility functions (k early) vs command helpers (opts/cfg context early, k late). Both are consistent within their respective groups.

### 20. ~~Silent time-parse failures~~ (FIXED)
`internal/cli/prune.go` now logs a warning when `okdev.io/last-attach` cannot be parsed and falls back to `CreatedAt`.

### 21. ~~No response body size limit on syncthing downloads~~ (FIXED)
`internal/syncthing/installer.go` now applies explicit size limits to checksum and archive downloads.

---

## Summary

| Category | P0 | P1 | P2 | P3 |
|---|---|---|---|---|
| Bugs/Correctness | 0 | — | — | — |
| Performance | — | 0 | — | — |
| Architecture/Tech Debt | — | — | 0 | — |
| Code Quality | — | — | — | 0 |

All findings from this review pass have been addressed.
