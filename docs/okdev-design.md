# okdev Design Doc

## 1. Overview

`okdev` is a lightweight, Kubernetes-native development environment tool for AI/LLM infra engineers.

It intentionally avoids:
- Okteto-style platform coupling and heavy cloud assumptions
- DevPod-style UI dependency and single-machine-bound workflows

It centers on one idea:
- **A Kubernetes PodSpec (with minimal okdev metadata) defines the full dev environment**

Design goals:
- Simple CLI + Kubernetes API integration
- Shareable dev sessions across machines and teammates
- Works with existing clusters, namespaces, RBAC, and admission policies
- Reproducible LLM infra dev stacks (GPU, model caches, large datasets, sidecars)

---

## 2. Non-Goals

- Building a full PaaS control plane
- Replacing CI/CD or GitOps deployment workflows
- Managing production serving stacks
- Forcing a hosted backend or mandatory account system

---

## 3. User Personas

### 3.1 Primary
AI infra engineer working on:
- model serving runtime
- inference optimization
- distributed training/inference pipelines
- data preprocessing and eval tooling

Needs:
- fast iteration against real k8s resources
- optional GPU access
- robust file sync and terminal access
- persistent caches/artifacts
- ability to continue work from different laptops/desktops

### 3.2 Secondary
Platform engineer maintaining shared dev clusters and guardrails.

Needs:
- namespace/RBAC compatibility
- policy-friendly manifests
- transparent, auditable behavior

---

## 4. Product Principles

1. **PodSpec-first**: no custom DSL required for core environment definition.
2. **Stateless client**: source of truth lives in Git + cluster objects, not local hidden state.
3. **Kubernetes-native identity**: contexts, kubeconfig, namespaces, service accounts.
4. **Explicit over implicit**: predictable commands and clear object ownership.
5. **Low cognitive load**: a small command set that covers 80% of daily dev.

---

## 5. UX Summary

Core flow:
1. Developer adds `.okdev.yaml` to repo, embedding or referencing PodSpec.
2. `okdev up` creates/resumes a dev session in a namespace.
3. `okdev connect` opens terminal/SSH and optionally starts port forwards.
4. `okdev sync` mirrors local repo <-> pod workspace.
5. Developer can leave and later reconnect from another machine via `okdev up --attach`.
6. `okdev down` stops session (or deletes, based on policy).

---

## 6. Configuration Model

Default config file: `.okdev.yaml` (minimal wrapper around PodSpec).

Config discovery order:
1. `-c, --config <path>` flag (highest priority)
2. `.okdev.yaml` in current repo root
3. `okdev.yaml` in current repo root (legacy/explicit fallback)

This keeps the default unobtrusive while preserving explicit naming when needed.

Example `.okdev.yaml`:

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-runtime-dev
spec:
  namespace: ai-dev
  workspace:
    mountPath: /workspace
    pvc:
      size: 200Gi
      storageClassName: fast-ssd
  session:
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
  sync:
    engine: native # native | syncthing
    paths:
      - .:/workspace
    exclude:
      - .git/
      - .venv/
      - node_modules/
  ports:
    - name: api
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
  ssh:
    user: root
    remotePort: 22
    localPort: 2222
  podTemplate:
    metadata:
      labels:
        app: okdev
        workload: llm-infra
    spec:
      serviceAccountName: ai-dev-sa
      containers:
        - name: dev
          image: ghcr.io/acme/llm-dev:cuda12.4-py311
          command: ["sleep", "infinity"]
          resources:
            requests:
              cpu: "8"
              memory: 32Gi
              nvidia.com/gpu: "1"
            limits:
              cpu: "16"
              memory: 64Gi
              nvidia.com/gpu: "1"
          volumeMounts:
            - name: workspace
              mountPath: /workspace
            - name: hf-cache
              mountPath: /cache/huggingface
      volumes:
        - name: workspace
          persistentVolumeClaim:
            claimName: llm-runtime-dev-workspace
        - name: hf-cache
          persistentVolumeClaim:
            claimName: hf-cache-shared
```

Notes:
- `podTemplate.spec` intentionally mirrors `core/v1.PodSpec`.
- okdev-specific features stay in `workspace`, `session`, `sync`, `ports`.

---

## 7. System Architecture

### 7.1 Components

1. **okdev CLI**
- Reads config
- Resolves kube context and namespace
- Creates/updates k8s objects
- Manages sync/port-forward/session attachment

2. **Session objects (k8s-native)**
- Pod (or Deployment/StatefulSet later)
- PVC(s) for workspace/cache
- ConfigMap/Secret for metadata and credentials
- Lease annotation/label for ownership and heartbeat

3. **Optional in-pod helper (small sidecar)**
- Handles optimized sync endpoint and health checks
- Optional; first version can use direct rsync/tar over exec

### 7.2 Why no mandatory controller in v1

Avoiding a custom operator keeps setup simple and portable:
- lower operational overhead
- easier to adopt in locked-down enterprise clusters
- fewer cluster-wide permissions

A controller/operator can be added later for advanced fleet features.

---

## 8. Kubernetes Resource Strategy

For each environment `metadata.name = X`:

- Pod: `okdev-X`
- Workspace PVC: `okdev-X-workspace` (unless explicit existing claim)
- Optional cache PVCs: user-provided
- Labels:
  - `okdev.io/name: X`
  - `okdev.io/owner: <user-or-team>`
  - `okdev.io/repo: <repo-slug>`
- Annotations:
  - TTL and idle timeout metadata
  - last attach timestamp

Ownership model:
- Team-shared session: explicit `shareable: true`, reusable by multiple clients.
- Personal session: default naming with user suffix (e.g. `okdev-X-alice`).

---

## 9. Command Surface (v1)

- `okdev init`
  - scaffolds `.okdev.yaml` from template and detects language/runtime hints
  - supports `-c, --config` to generate a custom config filename/path

- `okdev up [--attach] [--name] [-c|--config]`
  - create or resume session
  - wait for Pod Ready (watch-based readiness events)
  - optionally connect immediately
  - with `--attach`, auto-start configured port-forwarding and native watch sync in background
  - supports `--dry-run` to preview operations without applying
  - supports explicit `--session <name>` for multiple concurrent sessions per repo

- `okdev connect [--shell] [--cmd]`
  - attach shell/command in main container
  - supports reconnect across machines

- `okdev ssh [--setup-key] [--cmd]`
  - SSH over auto-managed `kubectl port-forward`
  - optional in-pod public key bootstrap

- `okdev sync [--mode=bi|up|down] [--engine=native|syncthing] [--watch] [--background] [--dry-run]`
  - native engine: tar stream over exec (single-shot or watch loop)
  - syncthing engine: continuous sync for single path mapping
  - supports detached syncthing mode via `--background`
  - exclude support, `.stignore` generation for syncthing

- `okdev ports`
  - establish all declared forwards

- `okdev status`
  - show pod state, sync health, forwarded ports, idle timer

- `okdev list`
  - list all sessions in current namespace/context (or across namespaces with flag)
  - includes repo, branch, owner, age, status, and lock mode

- `okdev use <session>`
  - set active session in local repo metadata for shorter commands
  - equivalent to passing `--session` on each command

- `okdev down [--delete-pvc]`
  - stop session; default preserves PVC for continuity
  - always releases the session Lease immediately for fast handoff/restart
  - supports `--dry-run` to preview deletions

- `okdev prune`
  - cleanup expired/idle sessions by TTL rules
  - enforces idle timeout from heartbeat (`okdev.io/last-attach`)

---

## 10. Session Sharing Across Machines

Problem with local-only tools: state is tied to one machine.

okdev approach:
- Session identity is cluster-native (`namespace + env name + labels`).
- Local machine stores only ephemeral client metadata.
- Any machine with kube access and repo can run `okdev up --attach`.

Optional lock modes:
- `none` (default shared)
- `advisory` (warn if another active client)
- `exclusive` (single active client lease with timeout)

Lease behavior:
- Lease duration is short-lived (default 2 minutes) and renewed in background for long-running commands (`connect`, `ssh`, `ports`, `sync --watch`, `sync --engine syncthing`, and `up --attach`).
- `okdev down` deletes the Lease object so exclusive sessions can be reacquired immediately.

## 10.1 Multi-Session Model (Multi-Project and Multi-Branch)

`okdev` should support many active sessions simultaneously, including:
- different repositories
- multiple branches of the same repository
- personal and shared team sessions

Session identity:
- `cluster-context + namespace + session-name`

Recommended default session naming:
- `<repo>-<branch>-<user>` for personal sessions
- `<repo>-shared` for team sessions

Examples:
- `serving-main-alice`
- `eval-feature-batching-alice`
- `llm-runtime-shared`

Config defaults:
- `.okdev.yaml` can define `spec.session.defaultNameTemplate`
- CLI can override with `--session`

Behavior rules:
- Every command accepts `--session`; if omitted, use active local session pointer.
- Active local pointer is stored in repo metadata (for convenience only).
- Commands never rely on hidden global state; explicit flags always win.
- `okdev sync` and `okdev ports` are scoped per session and can run concurrently.
- `okdev status --all` shows all sessions for fast switching.

Resource naming:
- Pod: `okdev-<session>`
- Workspace PVC: `okdev-<session>-workspace`

This guarantees isolation between projects while keeping shared cluster usage simple.

---

## 11. LLM Infra-Specific Design Choices

1. **GPU-first support**
- Direct PodSpec resource requests/limits
- Compatible with NVIDIA device plugin and node selectors/taints

2. **Persistent model/data caches**
- First-class PVC mounting for `HF_HOME`, tokenizer caches, compiled kernels
- Reduces cold-start time and egress cost

3. **Large artifact workflow**
- Encourage object-store mounts/clients rather than syncing huge datasets
- Sync excludes for checkpoints by default templates

4. **Multi-container dev pod pattern**
- Main dev container + optional sidecars (vector DB, redis, tracing agent)
- Shared workspace volume

5. **Long-running experiments**
- `okdev detach` (implicit with ctrl-p/q style behavior) and reconnect
- Session survives client disconnect

6. **Okteto-like sync option**
- Optional Syncthing-based engine for continuous synchronization
- Enabled per environment (`spec.sync.engine`) or per command (`okdev sync --engine syncthing`)
- No manual user install required:
  - local Syncthing binary is auto-downloaded and checksum-verified into `~/.okdev/bin/...`
  - pod-side Syncthing is provided by an auto-injected sidecar container when engine is `syncthing`

---

## 12. Security and Governance

- Respect existing kube authn/authz; no custom auth layer
- Namespace-scoped operation by default
- No privileged containers by default templates
- Support imagePullSecrets pass-through in PodSpec
- Auditability via standard Kubernetes events + labels
- Optional policy mode:
  - deny hostPath
  - enforce resource limits
  - require non-root

---

## 13. Reliability and Failure Handling

- Reconcile-on-command model: each CLI call verifies and repairs expected resources.
- Safe idempotency: `okdev up` should be repeatable.
- Sync auto-retries on network blips.
- Port forwarding with exponential backoff.
- Clear failure classes:
  - config validation error
  - kube API/RBAC error
  - pod scheduling error
  - sync transport error

---

## 14. Implementation Plan

### Phase 1 (MVP)
- `okdev init`, `up`, `connect`, `down`, `status`
- Pod + workspace PVC lifecycle
- basic sync (rsync/tar+exec)
- port-forward support
- shareable attach from another machine

### Phase 2
- improved sync engine with change detection and conflict handling
- idle timeout and TTL pruning
- advisory/exclusive lease mode
- richer status diagnostics

### Phase 3
- optional lightweight controller for background garbage collection and policy enforcement
- team templates/catalogs
- IDE helper integrations (still optional, no UI hard dependency)

---

## 15. Proposed Tech Stack

- Language: Go
- CLI: Cobra
- K8s client: controller-runtime client or client-go dynamic + typed clients
- Config parsing: sigs.k8s.io/yaml + JSON schema validation
- Sync backend (MVP): rsync or tar stream over SPDY exec
- Distribution: single static binary

---

## 16. Open Questions

1. Should `okdev` introduce a CRD now or stay CRD-less in MVP?
2. Default session model: one per repo, or one per branch/user?
3. Preferred sync engine for cross-platform reliability?
4. Should idle detection be client-heartbeat based or pod-side activity based?
5. How strongly should GPU node placement be abstracted vs left fully in PodSpec?

---

## 17. Success Metrics

- Time from clean clone to connected dev shell < 5 minutes
- Reattach from second machine < 60 seconds
- Session recovery success after transient network failure > 95%
- Median cold-start reduction from persistent cache > 50% for model-heavy workflows

---

## 18. Summary

`okdev` should be a simple, portable, PodSpec-first developer environment tool for Kubernetes.

By keeping the control model lightweight and cluster-native, it can deliver the core benefits of remote dev environments without platform lock-in, while fitting the practical needs of LLM infra engineering (GPU, persistent caches, large artifacts, and multi-machine continuity).
