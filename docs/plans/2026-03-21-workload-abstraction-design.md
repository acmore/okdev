# Workload Abstraction Design

## Problem

The current `okdev` session model assumes:

- one session creates or manages exactly one Pod
- SSH, sync, readiness, and lifecycle all target that single Pod
- ownership and session discovery are Pod-centric

That model breaks down for:

- multi-Pod application layouts
- replicated worker patterns
- controller-owned Pods
- higher-level training and batch resources such as `Job` or `PyTorchJob`

In practice, users want `okdev` to create or manage a workload, then attach to
one selected Pod within that workload for interactive development.

## Goals

- Support session workloads beyond a single Pod
- Keep SSH, sync, ports, and lifecycle targeting one explicit interactive Pod
- Allow higher-level Kubernetes resources such as `Job` and `PyTorchJob`
- Preserve the current simple Pod flow as the default and easiest path
- Make multi-Pod selection rules explicit and debuggable

## Non-Goals

- Attaching to multiple Pods simultaneously in one `okdev ssh` session
- Multi-Pod sync fan-out
- Generic deep merge of arbitrary manifest structures
- Replacing the existing Pod path immediately

## Core Model Change

Move from:

```text
session == Pod
```

to:

```text
session == workload
interactive target == one Pod/container chosen from that workload
```

This separates:

- workload creation and ownership
- Pod discovery
- interactive target selection

## Terminology

### Workload

A top-level resource managed by `okdev`, such as:

- Pod
- Job
- StatefulSet
- `PyTorchJob`
- an arbitrary controller-backed manifest in generic mode

### Target Pod

The concrete Pod used for:

- SSH
- sync
- port forwarding
- post-attach behavior

### Target Container

The container inside the target Pod used for interactive operations.

## Proposed Config Shape

Keep existing shared sections:

- `sync`
- `ports`
- `ssh`
- `lifecycle`
- `session`

Add a new top-level `workload` section:

```yaml
workload:
  type: pod
```

For a simple Pod workload:

```yaml
workload:
  type: pod
  pod:
    template:
      spec:
        containers:
          - name: app
            image: python:3.12
```

For a Job workload:

```yaml
workload:
  type: job
  manifestPath: .okdev/job.yaml
  inject:
    - path: spec.template
```

For an arbitrary manifest-backed workload:

```yaml
workload:
  type: generic
  manifestPath: .okdev/my-controller.yaml
  inject:
    - path: spec.template
      attachable: true
```

The `inject[].path` points to a PodTemplateSpec within the manifest. okdev
injects into `<path>.metadata.labels` (okdev session and ownership labels) and
`<path>.spec` (sidecar container, volumes, mounts) by refactoring the current
Pod-only injection logic into workload-aware helpers. This cannot rely on the
current `PreparePodSpec` unchanged because controller-backed workloads may use
arbitrary interactive container names rather than a fixed `dev` container.

For a higher-level controller with multiple role templates:

```yaml
workload:
  type: pytorchjob
  manifestPath: .okdev/pytorchjob.yaml
  inject:
    - path: spec.pytorchReplicaSpecs.Master.template
      attachable: true
    - path: spec.pytorchReplicaSpecs.Worker.template
      sidecar: false
      attachable: false
```

When `sidecar: false`, okdev injects only labels and storage mounts into that
template but omits the okdev-sidecar container. This is useful for worker Pods
that need the workspace volume but are never interactive targets.

`attachable` controls whether Pods created from that template are eligible for
interactive target selection.

For explicit interactive target container selection:

```yaml
workload:
  # ...
  attach:
    container: trainer
```

Label-based pod selection (`podSelector.matchLabels`) is deferred to a future
iteration. Target pod selection currently uses the `attachable` flag on inject
paths combined with the deterministic fallback rules.

For explicit storage intent:

```yaml
storage:
  workspace:
    mode: persistent
    mountPath: /workspace
    pvc:
      createIfMissing: true
      size: 50Gi
      storageClassName: fast
    attachTo:
      roles: [master]
```

### Inject Path Model

Each entry in `inject` declares a path to a PodTemplateSpec inside the
workload manifest. okdev uses these paths to perform targeted injection
without deep-merging or understanding the full CR structure.

For each inject entry, okdev:

1. Resolves the path within the loaded unstructured manifest
2. Extracts the object at that path as a PodTemplateSpec
3. Injects okdev labels into `.metadata.labels`
4. Injects runtime behavior into `.spec` for attachable templates (sidecar,
   workspace/runtime mounts, env, lifecycle hooks) unless `sidecar: false`, in
   which case only labels and storage are injected
5. Writes the modified PodTemplateSpec back at the same path

If a path does not resolve, `okdev up` fails with an error naming the
unresolvable path and the manifest file.

Built-in workload types provide default inject paths:

- `pod`: no inject paths needed (okdev builds the PodSpec directly)
- `job`: defaults to `spec.template`
- `pytorchjob`: no default; the user must declare inject paths because role
  names vary per manifest
- `generic`: no default; the user must declare inject paths explicitly

Inject entries also need explicit interactive intent:

- `attachable: true` means Pods from that template may be selected as the
  interactive target
- `attachable: false` means Pods from that template are excluded from target
  selection

For v1:

- `sidecar: false` implies `attachable: false`
- `attachable: true` requires `sidecar: true`
- if `attachable` is omitted, it defaults to `true` when `sidecar` is not
  `false`

This keeps worker or helper templates out of SSH/sync target selection.

### Pod Discovery via Injected Labels

Because okdev injects `okdev.io/session=<session>` and
`okdev.io/managed=true` labels into every declared inject path, child Pods
created by the controller inherit these labels automatically.

Pod discovery uses these labels by default:

```text
okdev.io/managed=true
okdev.io/session=<session>
```

An explicit `podDiscovery.labelSelector` may be needed if the controller
strips or overwrites Pod template labels. This is deferred to a future
iteration. The current implementation discovers pods using only the injected
`okdev.io/managed` and `okdev.io/session` labels, which covers all supported
workload types.

### Backward Compatibility

If `workload` is omitted:

- treat the config as `workload.type=pod`
- map the current `podTemplate` and related behavior into the new Pod path

This keeps current configs working unchanged.

## Storage Model

Storage should move from Pod-specific wiring to workload-aware intent.

Today, `okdev` effectively assumes:

- volumes belong to one Pod
- PVC creation is a Pod-oriented concern
- workspace semantics are tied to that one Pod

With workload abstraction, storage should be modeled in three layers:

1. session-level storage intent
2. workload-level volume attachment
3. target-level mount validation

### Session-Level Storage Intent

The config should describe what storage the session needs, independent of the
workload kind.

Example:

```yaml
storage:
  workspace:
    mode: persistent
    mountPath: /workspace
    pvc:
      createIfMissing: true
      size: 50Gi
      storageClassName: fast
```

This describes:

- what storage object exists
- whether `okdev` owns creation
- where it should be mounted
- whether the storage is ephemeral or persistent

### Storage Modes

The first storage model should support:

#### `ephemeral`

- backed by `emptyDir`
- not preserved across workload recreation
- useful for simple transient development Pods

#### `persistent`

- backed by an existing or okdev-created PVC
- preserved across workload recreation
- should be the default path for an interactive workspace

#### `template-managed`

- the workload controller owns claim creation
- examples include `StatefulSet.volumeClaimTemplates`
- `okdev` validates and attaches to it conceptually, but does not create it

#### `external`

- existing non-PVC storage references
- examples: `nfs`, `configMap`, `secret`, or other Kubernetes-native volume
  sources
- `okdev` only wires or validates the reference

### Storage Topology

For multi-Pod workloads, storage needs a topology, not just a source.

Useful long-term topologies:

- `shared`
- `per-role`
- `per-replica`

For the first implementation, support only:

- `shared`
- `single-target`

That keeps the initial design manageable.

Example:

```yaml
storage:
  workspace:
    mode: persistent
    topology: shared
    mountPath: /workspace
    pvc:
      claimName: team-workspace
```

or:

```yaml
storage:
  workspace:
    mode: persistent
    topology: single-target
    mountPath: /workspace
    pvc:
      createIfMissing: true
      size: 50Gi
    attachTo:
      roles: [master]
```

### Workload-Level Attachment

Storage is projected into Pod templates through the same `inject` paths used
for sidecar injection. Each inject path that receives storage gets the
workspace volume and volume mount added to its PodSpec.

By default, all inject paths receive the workspace volume. To restrict storage
to specific templates, use `attachTo`:

```yaml
storage:
  workspace:
    mode: persistent
    mountPath: /workspace
    pvc:
      createIfMissing: true
      size: 50Gi
    attachTo:
      roles: [master, worker]
```

The `attachTo.roles` field filters by matching against the inject path names
or workload-specific role identifiers.

For the first `PyTorchJob` path, the default should be conservative:

- allow a shared PVC for all roles
- or allow attach only to the chosen primary role
- do not attempt per-replica claim generation initially

### Target-Level Validation

Once a target Pod/container is selected, `okdev` must validate:

- the selected container actually mounts the declared workspace volume
- the mount path matches the session storage intent
- sync is only enabled if the target has the mount

If target selection resolves to a Pod/container without the required mount,
`okdev ssh`, `okdev sync`, and `okdev up` should fail clearly.

### Storage Ownership

PVC creation should be handled by a storage reconciler, not embedded inside
every workload implementation.

That reconciler should only create storage explicitly owned by `okdev`, such as:

- `storage.*.pvc.createIfMissing=true`

It should not create storage for:

- controller-managed volume claim templates
- externally referenced claims
- arbitrary CRD-specific storage resources

### Recommended First Storage Scope

The first careful implementation should support:

- one declared workspace storage object
- `emptyDir` or PVC-backed storage
- optional `createIfMissing`
- `shared` or `single-target` topology only
- role-based attachment for controller-backed workloads
- target mount validation before SSH/sync start

This is enough for Pod, Job, and an initial `PyTorchJob` implementation without
overcommitting to distributed storage orchestration.

## Architecture

## High-Level Flow

### `okdev up`

1. Load config and resolve workload type
2. Reconcile okdev-owned storage declared by the session
3. Build or load the top-level workload manifest from `manifestPath`
4. For each declared inject path:
   a. Resolve the path within the manifest to a PodTemplateSpec
   b. Inject okdev labels into `.metadata.labels`
   c. Run the refactored PodSpec injection helper on `.spec` (sidecar +
      storage), or storage-only if `sidecar: false`
   d. Write the modified PodTemplateSpec back
5. Apply the workload
6. Wait for the workload to produce candidate Pods (discovered via okdev labels)
7. Select the interactive target Pod/container
8. Validate target storage mounts
9. Wait for that target to become SSH-ready
10. Persist the selected target in session state
11. Configure SSH, sync, ports, and session state against the selected target

### `okdev ssh`

1. Resolve session
2. Load the workload descriptor and pinned target for that session
3. Rediscover current Pods owned by the workload
4. Verify that the pinned target still exists and is still attachable
5. Validate required target storage mounts
6. Connect to that target

If the pinned target no longer exists or no longer satisfies the attach
contract, `okdev ssh` fails clearly instead of silently selecting a new Pod.

### `okdev status`

Show both:

- workload summary
- interactive target summary

For multi-Pod workloads, also list discovered Pods and their roles/phases.

## Workload Interface

Introduce a workload abstraction in the CLI/runtime layer:

```go
type WorkloadRuntime interface {
    Kind() string
    Apply(ctx context.Context, k Client, ns string) error
    Delete(ctx context.Context, k Client, ns string) error
    WaitReady(ctx context.Context, k Client, ns string) error
    ListPods(ctx context.Context, k Client, ns string) ([]kube.PodSummary, error)
    SelectTarget(ctx context.Context, k Client, ns string) (TargetRef, error)
}
```

Where:

```go
type TargetRef struct {
    PodName    string
    Container  string
    Role       string
}
```

`SelectTarget` calls `ListPods` internally. The workload type knows best how
to discover and filter its own Pods, so the caller does not need to pass
candidates in. `SelectTarget` is used when establishing or explicitly changing
the interactive target, such as during `okdev up` or a repin operation.

The important points are:

- `Apply` loads the manifest, injects labels/sidecar/storage at the declared
  inject paths, and applies the result
- `ListPods` discovers child Pods using the injected okdev labels
- `SelectTarget` chooses the initial pinned interactive target
- For the Pod workload type, `Apply` builds the PodSpec directly using
  existing logic and the inject path model is not used

The storage path should be parallel to this, not embedded inside it:

```go
type StorageReconciler interface {
    Reconcile(ctx context.Context, k Client, ns string, storage StorageSpec) error
    ValidateTarget(ctx context.Context, k Client, ns string, target TargetRef, storage StorageSpec) error
}
```

## Workload Types

### 1. Pod Workload

This is the compatibility path.

Behavior:

- `okdev` still builds the Pod template directly
- `ListPods` returns that one Pod
- `SelectTarget` returns it

This should remain the default implementation.

### 2. Job Workload

Behavior:

- `okdev` loads the Job manifest from `manifestPath`
- default inject path: `spec.template`
- okdev injects labels, sidecar, and storage at the inject path
- Pod discovery uses injected okdev labels
- target selection defaults to the first Running Pod

This is useful for batch or bootstrap workflows.

### 3. PyTorchJob Workload

Behavior:

- `okdev` loads the `PyTorchJob` CR manifest from `manifestPath`
- inject paths must be declared explicitly (role names vary per manifest)
- okdev injects labels and sidecar/storage at each declared inject path
- Pod discovery uses injected okdev labels
- target selection defaults to a primary role such as `Master`

The key distinction is:

- `okdev` manages the top-level CR
- injection is path-based and does not require understanding the full CR schema
- interactive operations still target one concrete Pod

### 4. Generic Manifest-Backed Workload

Behavior:

- `okdev` loads an arbitrary top-level manifest from `manifestPath`
- inject paths must be declared explicitly
- okdev injects labels and sidecar/storage at each declared inject path
- Pod discovery uses injected okdev labels or an explicit discovery selector
- target selection uses explicit `attach` config or deterministic fallback

This is the escape hatch for arbitrary CRDs or controller-backed resources that
do not merit first-class workload semantics yet.

The key limitation is that `generic` provides structural support only:

- apply/delete of the top-level resource
- path-based PodTemplateSpec injection
- Pod discovery
- target selection
- single-target SSH/sync/ports

It does not imply workload-specific readiness semantics, role inference, or
controller-specific UX.

## Syncthing Model Under Workload Abstraction

Syncthing should remain single-target in the first version, even when the
workload has multiple Pods.

That means:

- one workload may produce many Pods
- one target Pod/container is selected for interaction
- one remote Syncthing endpoint is used, attached to that target

This keeps sync aligned with SSH and avoids turning the first workload
abstraction into a distributed file replication project.

### Current Assumption

Today, Syncthing assumes:

- a single session Pod
- one pod-side Syncthing instance
- one local-to-remote folder mapping
- one remote workspace mount

That maps well to the selected target Pod model, but not to multi-Pod fan-out.

### Proposed Sync Rule

For workload abstraction v1:

- `okdev sync` always targets the selected interactive Pod
- the selected Pod must mount the declared workspace storage
- other Pods are out of scope for active sync

This gives predictable behavior:

- `okdev ssh` and `okdev sync` land on the same target
- users do not need to reason about multiple concurrent remote sync endpoints

### Why Not Multi-Pod Syncthing First

Multi-Pod Syncthing introduces several hard problems:

- one folder replicated into multiple remote Pods may not be safe unless the
  backing storage is shared
- per-Pod Syncthing identities and cluster topology become part of the session
  model
- conflict semantics become unclear when workers write independently
- lifecycle cleanup becomes much harder for controller-backed workloads

That is too much complexity for the first workload abstraction.

### Supported Sync Cases

#### Case 1: Shared Storage, Single Target Sync

- a shared PVC is mounted by several Pods
- Syncthing syncs into the selected target Pod only
- other Pods see the same data through the shared volume

This is the best first multi-Pod experience.

It works well for:

- `Job`
- `PyTorchJob` where master and workers share one workspace volume

#### Case 2: Single-Target Storage

- only the selected target Pod mounts the workspace storage
- Syncthing writes to that target only

This is also safe and easy to reason about.

### Unsupported Initial Cases

Do not support initially:

- one Syncthing mesh across multiple workload Pods
- automatic per-worker sync endpoints
- multiple concurrent remote folder targets for one session
- sync into Pods that do not expose the interactive target storage

### Workload-Level Syncthing Injection

Sidecar injection is controlled by the `inject` paths in the workload config.
Only inject paths with `sidecar: true` (the default) receive the okdev-sidecar
container, which includes the Syncthing runtime.

Inject paths with `sidecar: false` receive only labels and storage mounts.
This keeps sidecar and sync overhead off Pods that are never intended for
interactive use.

For Pod and Job workloads:

- the single inject path gets the full sidecar by default

For `PyTorchJob`:

- the user controls which role templates get the sidecar via `inject` entries
- typically only the primary/interactive role gets `sidecar: true`

### Sync Eligibility Validation

Before `okdev sync` starts, validate:

- the workload has a selected target
- the selected target container mounts the required workspace path
- the selected target exposes the sync runtime expected by the engine

If those checks fail, `okdev sync` should reject the workload instead of trying
to guess another Pod.

### Future Extension Path

If multi-Pod sync is needed later, it should be a deliberate second design with
an explicit mode such as:

```yaml
sync:
  topology: target
```

or later:

```yaml
sync:
  topology: shared-volume
```

or:

```yaml
sync:
  topology: per-role
```

But the first implementation should hardcode the effective topology to:

- `target`

### Recommended Initial Syncthing Scope

The first workload-aware sync implementation should support:

- one selected target Pod
- one selected target container
- one remote folder mapping
- shared-volume visibility when several Pods mount the same workspace
- no multi-Pod Syncthing mesh

That keeps sync behavior understandable and consistent with the rest of the
interactive session model.

## Target Selection Rules

Target selection must be deterministic, visible to the user, and sticky once a
session is established.

Priority:

1. Explicit CLI override
2. Explicit config selector
3. Workload-type default role
4. Deterministic fallback

### CLI Overrides

Examples:

- `okdev ssh --pod <name>`
- `okdev ssh --role master`
- `okdev status --all-pods`

### Config Selection

```yaml
workload:
  attach:
    container: trainer
```

Target pod selection is driven by `attachable` flags on inject paths and the
deterministic fallback rules. Label-based pod selection (`podSelector`) is
deferred to a future iteration.

### Workload Defaults

- Pod: the only Pod
- Job: the first Ready Pod
- PyTorchJob: `Master` role first

### Deterministic Fallback

If multiple Pods still match:

- prefer Running over Pending
- prefer Ready over not Ready
- then sort by creation time, newest first
- then sort by name for tie-breaking

### Sticky Target Rule

After `okdev up` selects a target, that `TargetRef` is pinned in session state.

- `okdev ssh`, `okdev sync`, and `okdev ports` reuse the pinned target
- they do not silently drift to a different Pod after a rollout, retry, or
  scale event
- if the pinned target disappears, the command fails clearly and asks the user
  to repin or re-run `okdev up`

An explicit override such as `okdev ssh --pod <name>` or a future
`okdev target set ...` flow may replace the pinned target intentionally.

## Ownership and Labels

Current ownership is Pod-centric. That should become workload-centric.

Required labels on the top-level resource and on each declared inject path's
`.metadata.labels`:

- `okdev.io/managed=true`
- `okdev.io/session=<session>`
- `okdev.io/name=<config-name>`
- `okdev.io/owner=<owner>`
- `okdev.io/repo=<repo>`
- `okdev.io/workload-type=<type>`

This allows:

- session discovery from the workload
- Pod discovery through labels inherited from injected Pod templates
- `list`, `status`, `down`, and `prune` to work consistently

For controller-backed workloads, these labels are injected at the declared
inject paths, so child Pods inherit them automatically. No separate
label-injection logic is needed per workload type.

## Readiness Model

Readiness must stop meaning only “Pod Ready”.

Proposed readiness phases:

1. Workload applied
2. Candidate Pods discovered
3. Target Pod selected
4. Target Pod Ready
5. SSH target reachable
6. Optional sync/bootstrap complete

For `PyTorchJob`, workload ready does not mean all workers are globally ready.
For interactive use, the correct threshold is:

- the selected target Pod is ready enough to attach

## SSH and Sync Behavior

SSH, sync, and port forwarding remain single-target in the first design.

That means:

- one session maps to one workload
- one live interactive target Pod is chosen
- `okdev ssh`, `okdev sync`, and `okdev ports` operate against that target

That target is the pinned session target, not a freshly re-selected Pod on each
command.

This avoids forcing the entire runtime model to become multi-host at once.

## Lifecycle Hooks

Lifecycle becomes split:

### Workload-Level Hooks

Hooks around applying or deleting the workload.

### Target-Level Hooks

Hooks that run inside the selected interactive target Pod/container.

Current `postCreate` and `preStop` behavior should continue to mean
target-level behavior unless explicitly extended.

## Deletion Behavior

`okdev down` should delete the top-level workload, not just the target Pod.

Examples:

- Pod workload: delete the Pod
- Job workload: delete the Job
- PyTorchJob workload: delete the `PyTorchJob`

This is essential; deleting only the selected Pod would allow the controller to
recreate it and leave the session half-alive.

## Status Output

Status should include:

- session
- namespace
- workload kind/name
- selected target Pod/container
- workload phase
- target Pod phase

For multi-Pod workloads, show a compact Pod table:

- Pod name
- role
- phase
- ready
- selected target marker

## PyTorchJob-Specific Design

For the first CRD-backed workload, use the path-based injection model.

### Input Model

```yaml
workload:
  type: pytorchjob
  manifestPath: .okdev/pytorchjob.yaml
  inject:
    - path: spec.pytorchReplicaSpecs.Master.template
      attachable: true
    - path: spec.pytorchReplicaSpecs.Worker.template
      sidecar: false
      attachable: false
  attach:
    container: trainer
```

### Responsibilities

- load the user-provided CR manifest from `manifestPath`
- inject okdev labels at each declared inject path
- inject sidecar and storage at inject paths where `sidecar` is not `false`
- inject only labels and storage at inject paths where `sidecar: false`
- discover child Pods using injected okdev labels
- select the interactive target using `attach` config or deterministic fallback

Only Pods from inject entries with `attachable: true` are eligible to become
the interactive target.

### Assumptions

- the cluster already has the `PyTorchJob` CRD/operator installed
- `okdev` does not own the operator lifecycle
- the user knows the Pod template paths within their CR manifest

### Initial Constraints

- support one primary attach role only
- support one target container only
- do not attempt worker-wide port forwarding or sync

## Migration Strategy

### Phase 1

- introduce `WorkloadRuntime` interface and `PodWorkload` implementation
- wrap the current Pod path behind the interface with zero behavior change
- preserve current CLI behavior

### Phase 2

- add `JobWorkload` with path-based injection at default `spec.template`
- add target selection, `TargetRef`, and status output for multi-Pod workloads
- implement `StorageReconciler` and the `storage` config section

### Phase 3

- add `PyTorchJob` support using user-declared inject paths
- validate label injection, sidecar injection, and target selection with a
  real operator

### Phase 4

- add `generic` manifest-backed workload support
- document it as structural support for arbitrary controllers/CRDs
- keep workload-specific readiness and UX limited to first-class built-ins

## Risks

### 1. Hidden Target Ambiguity

If target selection is implicit and not surfaced, users will not understand
which Pod `okdev ssh` or `okdev sync` is hitting.

Mitigation:

- always display the selected target in `up` and `status`

### 1a. Target Drift Across Commands

If target resolution happens repeatedly on every SSH or sync command, the
session may drift to a different Pod after controller churn.

Mitigation:

- pin the selected target in session state
- require explicit repinning when the target disappears

### 2. Controller Reconciliation Conflicts

For CRDs and Jobs, `okdev` cannot safely mutate live child Pods directly.

Mitigation:

- mutate only the top-level resource or Pod templates
- rediscover Pods after reconciliation

### 3. Inject Path Misalignment

If the user declares an inject path that does not match the actual CR
structure, okdev will fail at apply time. Operators that restructure their
CRD schema across versions could break existing configs.

Mitigation:

- fail clearly with the unresolvable path and manifest file in the error
- document known inject paths for supported CRDs
- the path model is explicit and debuggable, unlike deep merge

## Recommended First Implementation

The first careful implementation should be:

1. Introduce `WorkloadRuntime` interface + `PodWorkload` wrapping current code
2. Add `TargetRef`, sticky target session state, and target selection to status
   output
3. Add `JobWorkload` with path-based injection (tests multi-Pod discovery
   without CRD complexity)
4. Add `StorageReconciler` and the `storage` config section
5. Add `PyTorchJob` last (requires CRD, operator, cluster-level testing)

This separates the interface refactor from behavior changes and gets each
piece tested independently.

## Resolved Questions

- **Sidecar injection into `manifestPath` workloads:** okdev injects
  automatically using the path-based injection model. The user declares
  `inject[].path` entries pointing to PodTemplateSpec locations within the
  manifest. okdev injects labels, sidecar, and storage at those paths using a
  refactored PodSpec injection helper that can target arbitrary interactive
  container names. No deep merge or full CR schema understanding is required.
  Inject paths with `sidecar: false` receive only labels and storage.

- **Multiple candidates with no selection rule:** `okdev up` should fail with
  a clear error listing the candidate Pods. A silent deterministic default is
  a footgun — users need to know which Pod they are on.

- **Sticky vs reselected target:** Sticky. `okdev up` pins one target in
  session state. Later interactive commands reuse it until the user explicitly
  repins or the target disappears.

- **Sync for controller-backed workloads:** Enable sync, but only after target
  selection succeeds and mount validation passes. The validation gates in the
  design are sufficient.

- **Built-in enum vs plugin registry:** Built-in enum (`pod`, `job`,
  `pytorchjob`, `generic`) for now. The path-based injection model already
  supports arbitrary CRDs without a plugin system — the user declares the
  inject paths and uses `type: generic` when no first-class workload kind fits.
  A formal registry is premature until there are enough workload types and
  external contributor demand to justify it.

## Open Questions

- Should the `inject` paths support glob or wildcard patterns for CRDs with
  dynamic role names, or should every role template be listed explicitly?
- Should okdev validate that the manifest at `manifestPath` matches the
  declared `workload.type`, or trust the user?
