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

- `okdev init`, `up`, `status`, `ssh`, `exec`, `cp`, `sync`, `ports`, `down`, `prune`, `jobs`
- config discovery or `.okdev.yaml` / `.okdev/okdev.yaml`
- sync behavior, session reuse, port forwards, or SSH access
- manifest-backed workloads such as `job`, `generic`, or `pytorchjob`
- attachable pod behavior, multi-pod sessions, or inter-pod SSH
- exec fanout controls: pod grouping (`--group`), uploaded `--script`, background `--detach` jobs (`okdev jobs list`), or structured `--json` output

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
- `okdev ssh` is the okdev-managed interactive path; `ssh okdev-<session>` is the plain SSH host alias path. The interactive shell can be bash or zsh, and the okdev tmux prefix is `ctrl-a`.
- For sync, SSH, and port-forward issues, `okdev status --details` is usually the first useful diagnostic.
- For commands across pods, prefer `okdev exec` fanout: `--group`/`--workers`/`--role`/`--label`/`--pod` to target, `--script` for non-trivial pipelines, and `--detach` + `okdev jobs list` for long background runs.
- `okdev exec --json` is the scripting/agent path: it always returns a JSON array of per-pod envelopes `{pod, exit, stdout, stderr, status}` and `okdev` stays exit 0 whenever the JSON is produced, so branch on each envelope's `exit`/`error`, not on okdev's process exit.
- The envelope `status` separates "responded with empty output" from "never responded": `responded` (exit code is data, even non-zero), `unreachable`, `timeout`, `error`, `missing`. Add `--require-all` to make okdev exit non-zero unless every pod responded — the right default for multi-node result collection.
- On multi-pod sessions with `ssh.interPod: true`, `exec --json` and `jobs list` route through an in-cluster gateway pod (`spec.exec.fanoutMode: auto` by default): one apiserver exec instead of one per pod, fanout over the pod network. `--gateway <pod>` forces a specific gateway; `spec.exec.fanoutMode: direct` restores per-pod exec.
- okdev pre-flight exit codes: `74` = session not found (pods genuinely gone, recreate with `okdev up`), `78` = transient cluster-contact failure (retry — okdev already retried once). These go to stderr and bypass `--json`.
- On non-TTY stdout (pipes, CI, agents) `okdev exec` auto-suppresses the per-pod name prefix; pass `--no-prefix=false` to force it back.
