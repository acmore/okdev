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

Use `docs/troubleshooting.md` for current sync-specific guidance.

## Session Reuse vs Fresh Workload

If the user says the workload was unexpectedly reused:

- remember `okdev up` reuses an existing session workload by default
- if the desired workload shape changed, suggest `okdev up --reconcile`
- if they need a fresh workload name or clean recreation, suggest `okdev down && okdev up`

Do not jump straight to deleting cluster objects manually unless `okdev` workflows are already failing.

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

## When To Escalate To kubectl

Escalate to `kubectl` only when `okdev` output is not enough, for example:

- pod scheduling failures
- cluster RBAC failures
- image pull failures
- controller-side manifest issues

When you do escalate, keep the command narrow and explain why that cluster-side view is needed.
