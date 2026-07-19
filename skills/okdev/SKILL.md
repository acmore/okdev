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

- `okdev init`, `up`, `restart`, `status`, `ssh`, `exec`, `cp`, `sync` (including `sync pause`/`resume`/`wait`), `ports`, `down`, `jobs`, `env-diff`
- config discovery or `.okdev.yaml` / `.okdev/okdev.yaml`
- sync behavior, session reuse, port forwards, or SSH access
- manifest-backed workloads such as `job`, `generic`, or `pytorchjob`
- attachable pod behavior, multi-pod sessions, inter-pod SSH, or stable inter-pod addresses (`MASTER_ADDR`, host aliases)
- exec fanout controls: pod grouping (`--group`), uploaded `--script`, background `--detach` jobs (`okdev jobs list`), cross-pod cascade termination (`--kill-group-on-exit`), targeted log reads (`okdev jobs logs <job-id> --pod/--role --tail N --since 90s --grep <regex> --dedup`), blocking on a job or a log pattern (`okdev jobs wait <id> [--grep <regex>]`), pattern-based process kill (`--pkill`), GPU cleanup with verification (`--reset-gpu`), or structured `--json` output

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

- Config resolution: `--config` flag > `OKDEV_CONFIG` env var > walking up from cwd (`.okdev/okdev.yaml`, `.okdev.yaml`, `okdev.yaml` per dir, up to the outermost git root — submodules do not cut the walk short). Discovery cannot reach a repo that is not an ancestor of cwd: from scratch/experiment dirs use `--session <name>` (explicit, multi-session-safe) or set `OKDEV_CONFIG` (single-project loops; okdev warns if it overrides a config discoverable from cwd). Typo'd spec fields are ignored by parsing but `okdev up` warns about them with the known keys.
- `okdev up` reuses an existing session workload by default.
- Use `okdev up --reconcile` when workload-shaping config changed and the user wants those changes applied.
- For setup that must survive pod recreation (installed tools, builds), point users at `spec.lifecycle.postCreate` (target pod, pre-sync) and `postSync` (all pods, after code arrives) — both re-run automatically on recreated pods; do not suggest manual re-runs. When it is unclear what was installed by hand during debugging, `okdev env-diff` lists the drift against the post-hook baseline and emits a ready-to-paste postCreate draft; okdev also hints at hooks whenever an `exec` command is itself an install (apt/pip/conda/apk). `okdev status --details` shows per-pod hook progress (`pending|running|done|failed|stale`); `stale` means an in-place container restart wiped the hook's effects and the next `okdev up` re-runs it. Automation that fans work out to all pods right after `up` should use `okdev up --wait-hooks`, which keeps converging postSync until every pod (including controller-created late arrivals) is done.
- When a container died and the restart policy will not bring it back, `okdev restart [--yes]` is the one-command recovery: delete + recreate + full setup with the same config. If only one pod of a multi-pod session is broken, `okdev restart --pod worker-3 [--yes]` recreates just that pod (controller-backed workloads; PyTorchJob replicas need `restartPolicy: OnFailure`, the scaffold default) — other pods keep their caches and detached jobs. Pod names change on recreation, so prefer the short-name aliases from `okdev status` (`master-0`, `worker-1`) with `--pod` — they also work for `cp`, `jobs`, and `target set`.
- `okdev up` on a `job` or `pytorchjob` workload fails fast when a pod enters `PodFailed` before readiness. If that run also created or recreated the workload, okdev deletes the failed workload and clears local session state — the next `okdev up` starts fresh. Workloads that were reused as-is (no create/recreate this run) are left alone. For `pod` workloads okdev also returns early on `Failed` but does not auto-delete.
- `okdev sync` and `okdev sync wait` answer different questions — do not substitute one for the other. `okdev sync` is about the **channel**: it starts or repairs the transport, and returning successfully does NOT mean the latest edits are on the pod. `okdev sync wait` is about the **data**: it guarantees pending changes have fully propagated, and it never starts or repairs anything. The canonical sequence when the next step depends on latest files: edit → `okdev sync wait` → run; if `sync wait` reports a channel problem, repair with `okdev sync`, then `sync wait` again.
- When sync is dead or stale (e.g. after a laptop network drop), a plain `okdev sync` now detects the unhealthy channel and repairs it in place — recommend it before `okdev sync --reset` (which remains for forced resets and mesh repair). `okdev sync` and `okdev sync wait` share one health check and never contradict each other about the same channel.
- Before a local `git checkout`/rebase while a remote job is running, `okdev sync pause` freezes the channel so swapped files cannot land under the live job; `okdev sync resume` + `okdev sync wait` afterwards. Paused is an explicit state: `sync wait` fails fast pointing at resume, and plain `okdev sync` never overrides it (only `resume` or the next `okdev up` does).
- Before a detached launch, sync health is checked automatically: an unhealthy channel prints a stale-code warning; `okdev exec --detach --require-sync` refuses to launch until sync is healthy and fully converged — recommend it for launches where running stale code would waste a long run.
- Sync supports multiple mappings with per-path direction (`spec.sync.paths[].direction: bi|up|down`); `down` makes the pod the authority so local writes cannot clobber results. A mapping's local root may nest inside the primary root — okdev manages the exclusion and remote volumes automatically. "Not syncing" is often the configured direction or a managed exclude, not a fault; see `references/multipod.md` and `references/troubleshooting.md`.
- `okdev ssh` is the okdev-managed interactive path; `ssh okdev-<session>` is the plain SSH host alias path. The interactive shell can be bash or zsh, and the okdev tmux prefix is `ctrl-a`.
- Multi-node launch scripts should use the short-name aliases as in-cluster addresses (`MASTER_ADDR=master-0`) — okdev writes them into every pod's `/etc/hosts` on `up`/`restart`, so they survive recreations without `hostname -i` chasing. After a controller-driven pod recreation, run `okdev up` to refresh both hooks and aliases. Plain PyTorchJob training can also just use the operator-injected `MASTER_ADDR` env.
- For sync, SSH, and port-forward issues, `okdev status --details` is usually the first useful diagnostic.
- If a session vanished without an `okdev down`, plain `okdev status` appends its last known pod states and surviving cluster events (`Evicted`/`Preempted`/quota vs plain `Killing`) — read that post-mortem before recreating: resource evictions mean `okdev up` will likely fail again, cleanup deletions recreate safely. `--output json` returns it structured. Events expire ~1h, so diagnose promptly.
- `okdev exec -- cmd` without a pod selector runs on the **target pod only** (safe default for write commands); fanout is opt-in — `--all`/`--workers`/`--role`/`--label`/`--pod`/`--group` to target, `--script` for non-trivial pipelines, and `--detach` + `okdev jobs list` for long background runs. Commands that must reach every pod need an explicit `--all`.
- To kill processes by pattern, prefer `okdev exec --pkill '<pattern>' [--signal <sig>]` over a raw `exec -- pkill -f ...`: raw pkill can match the shell running the command itself (exit 143, trailing cleanup skipped) and needs the `pkill -f 'patter[n]'` bracket trick, while `--pkill` can never match okdev's exec machinery and follows the pkill exit convention (0 matched, 1 none). Detached jobs: `okdev jobs stop <job-id>` kills the whole process group.
- For multi-pod detached launches where one crashed rank must not leave the others hanging in rendezvous holding GPU memory, launch with `okdev exec --all --detach --kill-group-on-exit -- ...` (requires `spec.ssh.interPod: true`): any member's exit SIGKILLs the whole cross-pod group. `jobs list` shows `degraded(...)` when members died while others run — the cue to `jobs stop` or `--reset-gpu` manually (a whole-container death cannot broadcast its own cascade).
- To free GPUs between experiment rounds, prefer `okdev exec [--workers] --reset-gpu` over pattern kills: it kills every GPU-holding process tree (SIGKILL by group) and then **verifies** `nvidia-smi` is back to zero — exit 0 = verified clear (idempotent), 3 = no nvidia-smi, 4 = still busy with leftovers listed. `--pkill` kills by name without the GPU verify; `jobs stop` is the graceful path for detached jobs.
- `okdev exec --json` is the scripting/agent path: it always returns a JSON array of per-pod envelopes `{pod, exit, stdout, stderr, status}` and `okdev` stays exit 0 whenever the JSON is produced, so branch on each envelope's `exit`/`error`, not on okdev's process exit.
- The envelope `status` separates "responded with empty output" from "never responded": `responded` (exit code is data, even non-zero), `unreachable`, `timeout`, `error`, `missing`. Add `--require-all` to make okdev exit non-zero unless every pod responded — the right default for multi-node result collection.
- On multi-pod sessions with `ssh.interPod: true`, `exec --json` and `jobs list` route through an in-cluster gateway pod (`spec.exec.fanoutMode: auto` by default): one apiserver exec instead of one per pod, fanout over the pod network. `--gateway <pod>` forces a specific gateway; `spec.exec.fanoutMode: direct` restores per-pod exec.
- okdev pre-flight exit codes: `74` = session not found (pods genuinely gone, recreate with `okdev up`), `78` = transient cluster-contact failure (retry — okdev already retried once). These go to stderr and bypass `--json`. On repeated 78s, watch stderr for `okdev-transient-streak: count=<n> since=<t>` — it appears from the 3rd consecutive failure spanning ≥1 minute and means a sustained outage: stop retrying and escalate to a human instead of looping. Any successful okdev call clears the streak.
- Fanout exec exit semantics: command ran but exited non-zero somewhere → `COMMAND EXITED NON-ZERO:` summary, exit = remote status (single pod) or `1` (multi-pod); command could not be delivered to some pod (unreachable, timeout, container gone) → `FAILED:` summary, exit `69`. `69` means investigate the pods, any other non-zero means investigate the command.
- `okdev sync wait [--timeout 10m]` blocks until all sync mappings have zero pending bytes in both directions — the edit-run guarantee: `vim train.py && okdev sync wait && okdev exec -- python train.py`. It never starts or repairs sync; it fails fast with a state-accurate report ("not running" vs "running but unhealthy") and points at `okdev sync` to repair.
- On non-TTY stdout (pipes, CI, agents) `okdev exec` auto-suppresses the per-pod name prefix; pass `--no-prefix=false` to force it back.
