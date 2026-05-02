---
name: okdev
description: Use when helping someone use okdev to initialize, start, inspect, connect to, sync with, troubleshoot, or tear down Kubernetes dev sessions, including manifest-backed and PyTorchJob workloads.
---

# okdev

## Overview

Use this skill for end-user questions about the `okdev` CLI. Prefer `okdev`-native workflows over generic Kubernetes or SSH advice, and anchor answers in the repo docs before giving detailed guidance.

Primary docs:

- `docs/quickstart.md`
- `docs/command-reference.md`
- `docs/config-manifest.md`
- `docs/troubleshooting.md`

## When to Use

Use this skill when the request involves:

- `okdev init`, `up`, `status`, `ssh`, `exec`, `cp`, `sync`, `ports`, `down`, `prune`
- config discovery or `.okdev.yaml` / `.okdev/okdev.yaml`
- sync behavior, session reuse, port forwards, or SSH access
- manifest-backed workloads such as `job`, `generic`, or `pytorchjob`
- attachable pod behavior, multi-pod sessions, or inter-pod SSH

Do not use this skill for:

- editing the `okdev` source code itself
- generic cluster administration unrelated to `okdev`
- generic SSH or tmux advice that does not depend on `okdev`

## Working Style

1. Identify the workload shape first: simple pod vs manifest-backed workload.
2. Prefer `okdev` commands and documented config fields over low-level `kubectl` workarounds.
3. For troubleshooting, ask for the smallest `okdev` output that exposes state, usually `okdev status --details`.
4. Escalate to `kubectl` checks only when `okdev` output is not enough.
5. When the question is about multi-pod behavior, read `references/multipod.md`.
6. When the question is about symptoms or failures, read `references/troubleshooting.md`.
7. When the question is about normal usage flow, read `references/workflows.md`.

## Quick Heuristics

- Config discovery order is: explicit `--config`, `.okdev/okdev.yaml`, `.okdev.yaml`, `okdev.yaml`.
- `okdev up` reuses an existing session workload by default.
- Use `okdev up --reconcile` when workload-shaping config changed and the user wants those changes applied.
- Use `okdev sync --reset` when local/background sync state is stale but the workload itself is still the intended one.
- `okdev ssh` is the okdev-managed interactive path; `ssh okdev-<session>` is the plain SSH host alias path.
- For sync, SSH, and port-forward issues, `okdev status --details` is usually the first useful diagnostic.
