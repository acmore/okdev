# Codebase Review: okdev

~30k lines of Go across 2 binaries and 12 internal packages. Overall the codebase is well-structured with good test coverage and clean package boundaries. Below are the findings organized by priority.

---

## P0 — Bugs & Correctness

### 1. Race condition in proxy health watchdog
`internal/cli/proxy_health.go:53-57` — Time calculation happens before the zero-check, meaning `time.Since(time.Unix(0, 0))` runs unnecessarily and could produce misleading results.

### 2. Missing context propagation in HTTP fetch
`internal/config/template.go:144-158` — `fetchTemplateURL()` creates an `http.Client` without accepting a context. If the caller's context is cancelled, the HTTP request won't be interrupted. Should use `http.NewRequestWithContext()`.

### 3. Fragile nil guard in session view
`internal/cli/session_view.go:69-89` — `selectTargetPod()` would panic on empty `pods` slice. Currently safe only because the caller checks `len(pods) == 0` first — an implicit contract that's easy to break.

---

## P1 — Performance

### 4. Repeated Kubernetes client creation (PARTIALLY FIXED)
`internal/cli/common.go`, `internal/cli/ssh.go`, `internal/cli/connect.go`, `internal/cli/ports.go`, `internal/cli/sync.go`, `internal/cli/up.go` — a shared `commandContext` now creates and reuses one `kube.Client` per command in the main session-oriented flows. Some simpler commands still build their own client inline, but the repeated multi-step command paths no longer recreate the client several times during one execution.

### 5. ~~Shelling out to `ps` in a polling loop~~ (FIXED)
`processAlive()` now uses `os.FindProcess` + `Signal(0)` with an `isZombie()` guard that reads `/proc/<pid>/stat` on Linux (no subprocess) and falls back to `ps` only on macOS. `processLooksLikeSyncthingSync()` reads `/proc/<pid>/cmdline` on Linux with the same macOS fallback. The zombie check is necessary because `Signal(0)` succeeds for zombie processes on Unix.

### 6. Marshal-unmarshal round-trip for type conversion
`internal/workload/generic.go:234-256` — `decodePodTemplateSpec` and `encodePodTemplateSpec` serialize to YAML then deserialize back just to convert between `map[string]any` and typed structs. Consider using `runtime.DefaultUnstructuredConverter`.

### 7. `reflect.DeepEqual` on Kubernetes objects
`internal/kube/client.go:169-171,205,236,396,425,436` — Used 8 times for spec/label/annotation equality. Field-level comparison or `equality.Semantic.DeepEqual` from apimachinery would be faster and more precise.

### 8. Bubble sort instead of `sort.Ints()`
`internal/ssh/tunnel.go:801-815` — Comment says "keep dependency surface low" but `sort` is a stdlib package.

---

## P2 — Architecture & Tech Debt

### 9. Massive function bodies (PARTIALLY FIXED)
- `internal/cli/ssh.go` — ~~`newSSHCmd()` RunE closure was 258 lines with reconnection logic inline.~~ Reconnection logic extracted to `runSSHShellWithReconnect()` with `sshShellRunner` interface, covered by 5 unit tests. The RunE closure is now ~70 lines shorter.
- `internal/cli/up.go` — `newUpCmd()` now delegates to `runUp()` plus explicit `upValidate()`, `upReconcile()`, `upDryRun()`, `upWait()`, and `upSetup()` phases. The UI flow is preserved, but there is still follow-up cleanup available inside setup helpers and some command helpers.

**Remaining work:** Continue trimming helper complexity in the SSH and up setup flows, especially around port-forward lifecycle and target refresh/setup sequencing.

### 10. ~~Unused parameters~~ (FIXED)
`startSessionMaintenance()` and `startSessionMaintenanceWithClient()` no longer accept `cfg` or `renewLock`. All 4 callers (ssh.go, connect.go, ports.go, sync.go) updated.

### 11. Duplicated config/namespace/session resolution (PARTIALLY FIXED)
`internal/cli/common.go` now has `commandContext` + `resolveCommandContext()` and the repeated load-config → resolve-namespace → resolve-session → build-kube-client flow has been collapsed in `up`, `ssh`, `ssh-proxy`, `connect`, `ports`, and `sync`.

**Remaining work:** Extend the same pattern to the remaining commands that still resolve config and session state manually, such as `status`, `target`, `down`, and `prune`.

### 12. Hardcoded timeouts scattered across files (PARTIALLY FIXED)
```
common.go:28    — 5 * time.Minute (heartbeat)
common.go:379   — 5 * time.Minute (context timeout)
ssh.go:114      — 10 * time.Second (keepalive)
ssh.go:231      — 5 * time.Second (rapid failure window)
ssh.go:241      — 60 * time.Second, 20 failures (circuit breaker)
syncthing.go:180 — 300ms sleep
portforward_helper.go:50 — 200ms ticker
```
These are now mostly centralized in `internal/cli/timeouts.go`, and the touched `common`, `ssh`, `sync`, `up`, and `portforward_helper` paths have been updated to use named constants.

**Remaining work:** Sweep the rest of `internal/cli` for leftover literals and decide whether every remaining timing knob belongs in the shared timeout set.

### 13. `goto` in migration code
`internal/config/migrate.go:239` — Uses `goto podTemplateDone` to break nested loops. A labeled `break` or helper function would be more idiomatic.

### 14. Inconsistent HTTP client patterns
`syncthing/installer.go` correctly uses an injectable `httpDoer` interface. `config/template.go` creates a raw `http.Client` inline — inconsistent and harder to test.

### 15. No cache invalidation
`internal/kube/client.go:99-127` and `internal/workload/generic.go:165-188` both cache results (Kubernetes clientsets and parsed manifests) with no invalidation strategy. If the kubeconfig or manifest files change mid-session, stale data will be served.

### 16. Goroutine lifecycle management
`internal/cli/common.go:414-437` — Heartbeat goroutine is fire-and-forget with only context cancellation for cleanup. No `sync.WaitGroup` or completion signal — caller can't tell if heartbeat succeeded or is still running.

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
- `resolveSessionName()` vs `resolveSessionNameForUpDown()` vs `resolveSessionNameWithState()` — confusing overloading
- No consistent parameter ordering for `(ctx, k, namespace, pod)` across functions
- `refreshTargetRef()` vs `loadTargetRef()` — unclear distinction

### 20. Silent time-parse failures
`internal/cli/prune.go:62-69` — If the `okdev.io/last-attach` annotation fails to parse as RFC3339, it silently falls back to `CreatedAt` with no log. Hard to debug malformed annotations.

### 21. No response body size limit on syncthing downloads
`internal/syncthing/installer.go` pipes HTTP response bodies directly to disk without size limits. `config/template.go` correctly limits to 1MB. Should be consistent.

---

## Summary

| Category | P0 | P1 | P2 | P3 |
|---|---|---|---|---|
| Bugs/Correctness | 3 | — | — | — |
| Performance | — | 5 | — | — |
| Architecture/Tech Debt | — | — | 8 | — |
| Code Quality | — | — | — | 5 |

## Top 3 Recommended Actions — Status

1. **Extract `newSSHCmd()` reconnection logic** — DONE. Extracted to `runSSHShellWithReconnect()` behind `sshShellRunner` interface, with 5 unit tests covering clean exit, non-recoverable error, successful recovery, context cancellation, and rapid failure circuit breaker.
2. **Remove unused params from `startSessionMaintenance`** — DONE. Removed `cfg` and `renewLock` from both `startSessionMaintenance()` and `startSessionMaintenanceWithClient()`, updated all 4 callers.
3. **Replace `ps`-based process polling with direct syscall** — DONE. `processAlive()` now uses `os.FindProcess` + signal(0). `processLooksLikeSyncthingSync()` reads `/proc/<pid>/cmdline` with macOS fallback to `ps`.

## Next Recommended Actions

1. **Finish rolling `commandContext` out to the remaining commands** — the pattern is fixed in the main session commands but still duplicated elsewhere.
2. **Do a final timeout sweep** — `timeouts.go` exists now, but not every CLI literal has been normalized yet.
3. **Keep trimming helper complexity in SSH/up setup flows** — the top-level commands are much cleaner, but some setup helpers are still dense.
