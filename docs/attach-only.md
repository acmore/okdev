# Attach-only access to existing pods

Use `spec.attachOnly` when okdev should run commands, track detached processes or
copy files on pods that already exist, without deploying or owning their workload.
It is mutually exclusive with `workload`, `workloads` and `defaultWorkload`.
Ordinary workload configurations retain their existing behavior.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: external-access
spec:
  kubeContext: my-cluster
  namespace: research
  attachOnly:
    selector: app=my-existing-service
    container: app
    setup: 'test -d /workspace && command -v python3'
```

Exactly one scope is required:

| Field | Scope |
| --- | --- |
| `pods: [worker-0, worker-1]` | Exact existing pod names; all must exist |
| `selector: app=my-service` | Kubernetes label selector in the configured namespace |
| `session: training` | Existing pods labelled `okdev.io/managed=true,okdev.io/session=training` |

`container` is required. `setup` is an optional remote `sh -c` command. An empty
scope or a scope matching no pods fails. A selector is evaluated on each call, so
it may include newly replaced pods. The scope is not a snapshot of pod UIDs.
The `session` field matches live session labels, not a saved local run-ID pin.
`--session` or a positional session name identifies the local invocation; it does
not expand or replace the configured attach scope. Use `--config` explicitly;
attach-only access does not create saved session associations or target pins.

## Supported operations

```bash
okdev -c access.yaml validate
okdev -c access.yaml exec --pod worker-0 -- python3 --version
okdev -c access.yaml exec --all -- nvidia-smi
okdev -c access.yaml cp --all ./input.bin :/tmp/input.bin
okdev -c access.yaml exec --all --detach -- python3 /workspace/train.py
okdev -c access.yaml jobs list
okdev -c access.yaml jobs logs <job-id>
okdev -c access.yaml jobs stop <job-id>
```

`exec`, `jobs` (including wait/ready/logs/stop), `exec-jobs` and `cp` reuse their
normal command semantics. Selectors such as `--pod`, `--label` and `--exclude`
can only narrow the configured scope. With multiple pods in scope, exec/cp need
an explicit selection; they do not silently choose a target. `jobs` defaults to
all scoped pods. An explicit `--container` overrides the configured container.
Role/worker selection still uses okdev role labels; StatefulSet ordinals do not
imply Job/worker roles and okdev does not add those labels.

Every other command is refused with an `attach-only` error before any cluster
contact, `okdev status` and `okdev logs` included. Both report on a session okdev
deployed and owns: pinned target, sync channel, SSH alias, lifecycle hook state
and declared-versus-observed replicas. None of that exists for pods okdev did not
create, so the mode does not answer for them. Use `kubectl get pods` and
`kubectl logs` for pod state and container logs, `okdev jobs list` and
`okdev jobs logs` for detached jobs okdev itself started, and `okdev validate` to
check the config.

Commands run directly through Kubernetes exec, using the caller's kubeconfig
credentials and RBAC. An existing `okdev.io/owner` label must match the selected
owner identity, checked on **every** pod in the configured scope before access.
A foreign-owner member makes that scope fail even if command flags select a
smaller subset; narrow the config when access is intentionally limited.
Unlabelled external pods remain subject to Kubernetes RBAC. `--owner` retains its
normal identity-override semantics; it does not grant Kubernetes permissions.

## Lifecycle boundary and explicit setup

`up` (including dry-run), `down`, `restart` (including `--pod`), workload management,
sync and managed SSH/port setup are unavailable through this configuration.
The mode does not apply manifests, adopt controllers, inject sidecars, install
SSH/helpers, relabel pods, update hosts aliases or automatically run lifecycle
hooks. Existing `lifecycle.postCreate/postSync/preStop` entries do not run here.
It also does not manage Syncthing or check sync health before commands.
`--require-sync`, `--gateway`, nonzero `--preflight-retry-timeout`, interpod SSH and
gateway fanout configuration are unsupported in this initial mode.

Run the optional setup command explicitly:

```bash
okdev -c access.yaml attach-setup --pod worker-0
# Or deliberately run it on every scoped pod:
okdev -c access.yaml attach-setup --all
```

Each invocation executes `spec.attachOnly.setup` again; there is no automatic
once-per-pod hook marker. Make repeated setup safe when needed. Its commands can
modify files/processes in the container, just like explicit exec. Attach-only
prevents workload lifecycle operations through this config; it is not a sandbox
for remote commands or a replacement for Kubernetes authorization. Using a
separate deployable config still permits its ordinary lifecycle operations.

## Prerequisites and limits

The container must provide the tools needed by the requested operation. Ordinary
exec needs its command/shell. Copy paths need their existing shell/file/archive
tools (for example `sh`, `cat`, `wc`, `mktemp`, `mv` and `tar` for archives), and
verified downloads need SHA-256 tooling. Detached jobs need shell/process tools
including `nohup`, `ps`, `sed`, and a writable metadata directory
(`/var/okdev/exec`, falling back to `/tmp/okdev-exec`). Check container permissions
before relying on these paths. `setsid` enables process-group tracking; without it,
job stop can only signal the leader. No tools are silently installed.

Detached jobs are tracked **processes**, not Kubernetes Jobs. Metadata may be lost
when an external pod/container is replaced unless its filesystem is persistent;
there is no injected runtime volume or sidecar to preserve it. Job commands do
not discover arbitrary pre-existing processes as tracked jobs. They do not assume
that StatefulSet pods carry Job labels. Use the workload's own controller/tools
for pod creation, reconciliation, restarts and teardown.

The persistent Kind regression exercises two external StatefulSet pods without
okdev sidecars: exec/stream/exit/container behavior, file-copy hashes, detached
jobs, explicit setup, scope and owner rejection, Kubernetes RBAC denial, and
unchanged pod/controller identity, labels, ownership and specs.
