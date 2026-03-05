# okdev Implementation Plan

## 1. Goal

Build `okdev` as a lightweight, Kubernetes-native developer environment CLI for AI/LLM infra workflows, with PodSpec-first configuration, multi-session support, sync, port forwarding, and cross-machine reattach.

## 2. Delivery Strategy

- Ship a thin, reliable MVP first.
- Keep Kubernetes as the only provider/back-end.
- Prioritize correctness and usability over breadth.
- Add advanced features only after daily workflow is stable.

## 3. Milestones and Timeline

Assume 2-week sprints. Total: 8 weeks for MVP + hardening.

### Milestone 0 (Week 1): Project Bootstrap

Deliverables:
- CLI skeleton (`cobra`) with command groups
- Config loader for `.okdev.yaml` + `-c/--config`
- JSON schema validation for config
- Basic structured logging and error model

Definition of done:
- `okdev version`, `okdev init`, and config parse work locally
- Invalid config errors are actionable and include field paths
- CI runs lint + unit tests on PRs

### Milestone 1 (Weeks 2-3): Session Lifecycle Core

Deliverables:
- `okdev up`, `okdev down`, `okdev status`
- Session identity model (`context + namespace + session`)
- Pod + workspace PVC creation/reuse
- Multi-session support (`--session`, `okdev list`, `okdev use`)

Definition of done:
- Can create two sessions for two repos in same namespace without collision
- `okdev up` is idempotent
- `okdev status --all` reports all sessions accurately

### Milestone 2 (Weeks 4-5): Developer Connectivity

Deliverables:
- `okdev connect` (interactive shell/command execution)
- `okdev ports` with config-defined forwards
- Cross-machine reattach (`okdev up --attach`)
- Local active-session pointer in repo metadata

Definition of done:
- Reattach from second machine works with same kube credentials
- Port forwards auto-retry on transient disconnect
- Connect path supports long-running dev shells

### Milestone 3 (Weeks 6-7): Sync + LLM Infra Defaults

Deliverables:
- `okdev sync` modes: `bi`, `up`, `down`
- Exclude rules and resume behavior
- Default templates for LLM infra (`gpu`, `cache-pvc`, `sidecars`)
- Better diagnostics for scheduling/RBAC/sync failures

Definition of done:
- Typical Python/Go monorepo sync round-trip is stable
- Excludes prevent accidental large artifact sync
- GPU template works on clusters with NVIDIA device plugin

### Milestone 4 (Week 8): Hardening + v0.1.0 Release

Deliverables:
- `okdev prune` for TTL/idle cleanup
- Documentation set: quickstart, command reference, troubleshooting
- E2E tests on KinD/minikube + one managed cluster target
- Release pipeline for binaries (macOS/Linux)

Definition of done:
- v0.1.0 tagged and reproducible builds published
- MVP workflows validated end-to-end by at least 2 real projects

## 4. Workstreams (Parallel)

### A. CLI and UX
- Command signatures and help text consistency
- Human-readable + machine-readable output (`--output json`)
- Error taxonomy and exit codes

### B. Kubernetes Runtime
- Context/namespace resolution
- Pod/PVC lifecycle reconciliation
- Label/annotation conventions for discovery

### C. Sync Engine
- Initial backend: tar stream over exec
- File watcher + debounce loop
- Conflict policy for `bi` mode (MVP: last write wins + warning)

### D. Networking and Access
- Port forward manager
- Shell attach + command execution
- Session liveness checks

### E. Docs and Developer Experience
- `.okdev.yaml` schema reference
- Example configs: CPU-only, GPU, multi-container
- Ops guide for RBAC and policy constraints

## 5. Suggested Repository Layout

```text
cmd/okdev/
internal/config/
internal/cli/
internal/session/
internal/kube/
internal/sync/
internal/ports/
internal/connect/
internal/output/
docs/
```

## 6. Test Plan

### Unit tests
- Config parsing/validation
- Session naming and selector logic
- Command option precedence (`--session`, `-c/--config`)

### Integration tests
- Pod/PVC create/reuse/delete behavior
- Multi-session listing and switching
- Port-forward lifecycle

### End-to-end tests
- `init -> up -> connect -> sync -> status -> down`
- Cross-machine reattach simulation (two kubeconfigs/clients)
- GPU scheduling path (when GPU nodes available)

## 7. Risks and Mitigations

1. Sync reliability across large repos
- Mitigation: start simple, add resume and explicit excludes, benchmark early.

2. Cluster policy incompatibility (PSA/admission constraints)
- Mitigation: policy-safe default templates + clear validation warnings.

3. Session conflicts in shared namespaces
- Mitigation: strong session naming defaults + advisory/exclusive lease modes.

4. GPU scheduling unpredictability
- Mitigation: keep placement explicit in PodSpec and surface scheduling diagnostics.

## 8. MVP Scope Freeze

Must-have for v0.1.0:
- `.okdev.yaml` + `-c/--config`
- `up`, `status`, `connect`, `ports`, `sync`, `down`, `list`, `use`
- Multi-session support
- Cross-machine reattach
- Workspace PVC persistence

Explicitly out of scope for v0.1.0:
- Custom operator/controller
- Non-kubernetes providers
- Mandatory UI/IDE integration
- Advanced sync conflict resolution beyond warnings + basic policy

## 9. Immediate Next Actions (This Week)

1. Create Go module and CLI scaffolding.
2. Implement config loader + schema validation.
3. Implement session naming and object label conventions.
4. Implement `okdev up/status/down` happy path on KinD.
5. Add first quickstart doc with a GPU-capable sample `.okdev.yaml`.
