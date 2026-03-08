# Lifecycle Hooks & .okdev/ Folder Convention

## Problem

Teams need automated environment setup when creating dev sessions — installing dependencies, configuring tools, seeding data. Currently users must manually run setup steps after `okdev up`. There's also no convention for organizing okdev config alongside lifecycle scripts.

## Solution

Add `postCreate`, `postAttach`, and `preStop` lifecycle hooks that run in the dev container. Support `.okdev/` as a folder convention for config + scripts. Convention-based with config override.

## Design

### Config Resolution Order

1. `.okdev/okdev.yaml`
2. `.okdev.yaml`
3. `okdev.yaml` (legacy)

First found wins. Searched from cwd up to git root (existing behavior).

### Lifecycle Hooks

**`postCreate`** — Runs once after pod is first created and ready.
- Executed via `kubectl exec` in the dev container
- Convention: `.okdev/post-create.sh` (auto-detected if exists)
- Config override: `spec.lifecycle.postCreate: "make setup"`
- Pod annotation `okdev.io/post-create-done=true` prevents re-run
- Blocking — failure stops `okdev up` with an error
- Scripts are synced to the pod workspace before execution

**`postAttach`** — Runs every interactive SSH login.
- Executed in `nsenter-dev.sh` (sidecar ForceCommand)
- Convention: checks if `<workspace>/.okdev/post-attach.sh` exists in the dev container, runs it before the shell
- Non-blocking — failure prints warning, shell still opens

**`preStop`** — Runs before pod terminates.
- Wired as a Kubernetes `preStop` lifecycle hook on the dev container in the pod spec
- Convention: `.okdev/pre-stop.sh`
- Config override: `spec.lifecycle.preStop: "make clean"`
- Kubernetes handles execution and grace period

### Script Execution Flow (postCreate)

1. `okdev up` waits for pod ready
2. Sync workspace (if syncthing configured) — ensures `.okdev/` scripts are on the pod
3. Check pod annotation `okdev.io/post-create-done`
4. If not annotated, resolve script: config `spec.lifecycle.postCreate` → `.okdev/post-create.sh` convention
5. Execute via `kubectl exec` in the dev container
6. On success, annotate pod `okdev.io/post-create-done=true`
7. On failure, return error (blocks `okdev up`)

### Config Schema

```yaml
spec:
  lifecycle:
    postCreate: ".okdev/post-create.sh"  # optional, overrides convention
    preStop: ".okdev/pre-stop.sh"        # optional, overrides convention
```

`postAttach` has no config override — purely convention-based since it runs server-side in the sidecar.

### Components Modified

- `internal/config/loader.go` — Add `.okdev/okdev.yaml` to resolution order
- `internal/config/config.go` — Add `LifecycleSpec` struct to `DevEnvSpec`
- `internal/cli/up.go` — Run `postCreate` after pod ready + sync, annotate pod
- `internal/kube/podspec.go` — Wire `preStop` hook on dev container
- `internal/kube/client.go` — Add `AnnotatePod` method
- `infra/sidecar/entrypoint.sh` — Add `postAttach` check in `nsenter-dev.sh`

### Out of Scope

- `postStart` (Kubernetes-level, before pod is "ready" — too early for user scripts)
- Script timeout configuration (use Kubernetes `terminationGracePeriodSeconds` for `preStop`)
- Parallel script execution
