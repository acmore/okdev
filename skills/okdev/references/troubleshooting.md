# okdev Troubleshooting

## First Diagnostic

For most runtime issues, ask for:

```bash
okdev status --details
```

That is usually the highest-signal starting point for:

- sync health
- SSH host setup
- managed forwards
- session state

Then narrow further based on the symptom.

## Sync Problems

Common checks:

- does `okdev status --details` show active sync
- did the session pod get recreated
- is the remote workspace path writable
- is the sidecar image pull succeeding

Standard repair steps:

- `okdev sync --foreground`
- `okdev sync --reset`
- `okdev up` again if the pod was recreated

If a real file was overwritten by a bad write syncing across from the other side (e.g. an empty local file clobbered a pod-side result), recover the previous version from `.stversions/` at the sync root on the side that had the good copy (pod: `<remote workspace>/.stversions/`; local: `<local sync root>/.stversions/`). Versioning is on by default (`spec.sync.syncthing.versioningDays`, 30 days).

Before treating "changes are not syncing" as a fault, check two BY-DESIGN behaviors:

- **Direction**: with `direction: up` the pod's writes never reach local; with `down` local writes never reach the pod (syncthing flags them as local additions). `okdev status --details` shows each mapping's direction arrow. This is the configured contract, not a sync failure.
- **Managed excludes**: a directory nested inside the primary sync root that belongs to (or belonged to) its own mapping is excluded from the primary folder via the managed block in the primary root's `.stignore`. `status --details` lists active excludes and tombstones retained after a mapping was removed; deleting the entry from `.stignore` is the explicit way to fold the directory back into the primary folder.

Use `docs/troubleshooting.md` for current sync-specific guidance.

## Session Reuse vs Fresh Workload

If the user says the workload was unexpectedly reused:

- remember `okdev up` reuses an existing session workload by default
- if the desired workload shape changed, suggest `okdev up --reconcile`
- if they need a fresh workload name or clean recreation, suggest `okdev down && okdev up`

Do not jump straight to deleting cluster objects manually unless `okdev` workflows are already failing.

If setup (installed tools, builds) is lost after a pod recreation: that is overlay-filesystem lifetime, and the fix is `spec.lifecycle` hooks, not manual re-runs — `postCreate` (target pod, before sync; e.g. installing kubectl) and `postSync` (all pods, after code arrives; e.g. `pip install -e .`). Both re-run automatically on recreated pods because their done-markers are pod annotations that die with the pod.

## Auto-Cleared Failed Jobs / PyTorchJobs

If the user's previous `okdev up` failed with a pod in `Failed` state on a `job` or `pytorchjob` workload, okdev may have already deleted that workload and cleared local session state as part of the same `okdev up`. Symptoms:

- `okdev status --details` reports the session as gone (`okdev` pre-flight exit `74`)
- The next `okdev up` is expected to create a fresh workload, not "reconnect"
- The failed `okdev up` output includes a `Teardown` section that names the deleted workload

This auto-cleanup only fires when `okdev up` itself created or recreated the workload during that run. Workloads that were reused as-is are not auto-deleted, so `okdev up` on a pre-existing failed Job/PyTorchJob returns an error but leaves the workload in place for manual inspection.

`pod` workloads also fail fast on `Failed`, but okdev does not auto-delete them — inspect with `kubectl` if the pre-flight readiness error alone is not enough.

## SSH Problems

Common checks:

- `okdev ssh --setup-key`
- confirm the managed SSH host entry exists: `okdev-<session>`
- confirm `okdev-sshd` is running
- inspect `okdev status --details`

If the issue is specific to plain SSH vs managed SSH, distinguish:

- `okdev ssh` for the okdev-managed interactive path
- `ssh okdev-<session>` for the plain alias path

## Port Forward Problems

Common repair:

```bash
okdev ports
```

Use this when forwards need to be rewritten or re-established after workload or SSH changes.

## Local State

Remember that local runtime state lives under:

```text
~/.okdev/
```

If behavior looks stale, prefer targeted cleanup or reset guidance rather than broad destructive advice.

## Exit Codes And Scripting

When `okdev` is driven by scripts, CI, or agents, distinguish its pre-flight exit codes:

- `74` — session not found: the cluster was reachable but the session has no pods. It is genuinely gone; recreate with `okdev up`.
- `78` — transient cluster-contact failure (API timeout, connection refused/reset, overload). okdev already retried once with backoff, so the caller should retry rather than treat the session as dead.

`okdev exec --json` reports each pod's result as data (`{pod, exit, stdout, stderr}`) and exits 0 whenever the JSON document is produced — a non-zero *remote* exit does not make okdev exit non-zero. Branch on each envelope's `exit`/`error`. Only pre-flight failures (session not found / cluster unreachable) bypass JSON and surface on stderr with the 74/78 codes above.

## When To Escalate To kubectl

Escalate to `kubectl` only when `okdev` output is not enough, for example:

- pod scheduling failures
- cluster RBAC failures
- image pull failures
- controller-side manifest issues

When you do escalate, keep the command narrow and explain why that cluster-side view is needed.
