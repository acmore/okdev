# okdev Workflows

## First Look

Start by identifying:

- which config file is in play
- whether the workload is `pod` or manifest-backed (`job`, `generic`, `pytorchjob`)
- whether the user is trying to create a new session, reuse an existing one, or debug an existing one

Useful docs:

- `docs/quickstart.md`
- `docs/command-reference.md`
- `docs/config-manifest.md`

## Common Lifecycle

Typical flow:

1. `okdev init`
2. edit config or generated manifest if needed
3. `okdev up`
4. `okdev status --details`
5. connect with `okdev ssh` or `okdev exec`
6. `okdev down`

## Config Location

- Simple pod setups usually use `.okdev.yaml`.
- Manifest-backed workloads initialized by `okdev init` usually use `.okdev/okdev.yaml`.
- Generated companion manifests usually live beside that config under `.okdev/`.

If config-path confusion is possible, recommend `--config <path>` explicitly.

## Session Start

Use `okdev up` for the standard startup path.

Important behavior:

- `okdev up` creates or reuses the session workload
- starts `okdev-sshd`
- starts background Syncthing sync
- configures managed SSH host and port forwards

If the user expects workload-shaping changes to apply, check whether they need:

- `okdev up --reconcile`
- or `okdev down && okdev up` for a fresh workload

## Session Access

Use:

- `okdev ssh` for the managed interactive shell (bash or zsh; okdev tmux prefix is `ctrl-a`)
- `ssh okdev-<session>` for plain SSH
- `okdev exec -- cmd` for running a command on the target pod (selector-less default — fanout is opt-in), and multi-pod parallel execution with explicit targeting (`--all`/`--role`/`--label`/`--pod`/`--group`/`--workers`, `--script` to upload+run a file, `--detach` for background runs surfaced by `okdev jobs list`, `--pkill '<pattern>'` to kill matching processes without the raw-pkill self-match footgun, `--json` for structured per-pod results, and `--fanout` concurrency control)
- `okdev jobs logs <job-id>` accepts the same `--pod`/`--role`/`--label`/`--exclude` targeting as `okdev exec`; use it to tail a subset of a detached job's pods instead of every pod in the session
- for monitoring poll loops, prefer `okdev jobs logs <job-id> --tail 200 --since 90s`: `--tail` bounds the transfer, `--since` skips pods whose log file has not changed since the cutoff (file-level gate — job logs have no per-line timestamps). Reads are size-verified and retried, so an empty result means an empty/idle log, not a dropped stream
- to extract a metric or the first error cheaply, add `--grep`: `okdev jobs logs <id> --grep 'reward=|Error' --tail 5` returns just the latest matching lines (filtered pod-side). Add `--dedup` when multi-rank workers spam identical exception bodies — runs of identical lines collapse to one copy plus `[repeated Nx]`. Progress-bar logs (`\r` rewrites) are CR-normalized automatically on snapshot reads, so there is no need to drop to `okdev exec -- tr/grep` raw-file pipelines
- do NOT build `sleep`+`grep` poll loops to wait on a job: `okdev jobs wait <id>` blocks until the job finishes, and `okdev jobs wait <id> --grep 'step 2|Error'` blocks until the log matches (returns the matching line, works even if the job already exited). One blocking call replaces the whole loop
- between experiment rounds, do NOT hand-roll `pkill -9 -f "[W]orker|[v]LLM..."` + `nvidia-smi` checks: `okdev exec --workers --reset-gpu` kills every GPU-holding process tree in the targeted pods and verifies `nvidia-smi` is back to zero in one call (exit 0 = verified clear and idempotent, 3 = no nvidia-smi, 4 = still busy with leftovers listed). For killing by name pattern without the GPU verify, `--pkill '<pattern>'` remains the right tool

If attachable pods are involved, explain that interactive targeting follows attachable-pod selection rules rather than arbitrary pod choice.

## Sync

Default guidance:

- `okdev up` already starts sync and waits for initial convergence
- `.git` is synced by default
- `okdev status --details` shows whether sync is active
- `okdev sync --foreground` is useful when the user needs live troubleshooting
- two commands, two questions — never substitute one for the other:
  - `okdev sync` = **channel**: start or repair the transport. A successful return does NOT mean the latest edits are on the pod.
  - `okdev sync wait` = **data**: block until pending changes have fully propagated. It never starts or repairs the channel.
  - canonical edit-run sequence: `vim train.py && okdev sync wait && okdev exec -- python train.py`; if `sync wait` reports a channel problem, run `okdev sync` to repair, then `sync wait` again before running
- a plain `okdev sync` self-heals a dead or stale channel (health-checked, never a false "already running"); `okdev sync --reset` remains for forced resets and mesh repair
- before a local branch switch / rebase while a job is running: `okdev sync pause` → git operations → `okdev sync resume` → `okdev sync wait`. Paused is explicit — `sync wait` fails fast pointing at resume, plain `okdev sync` refuses to override it
- `okdev exec --detach --require-sync` gates a job launch on healthy + fully converged sync in one flag (unguarded detach launches warn on stderr when the channel is unhealthy)
- `okdev sync wait` blocks until every mapping has zero pending bytes both ways — use it between an edit and a run to guarantee the pod sees the latest files: `vim train.py && okdev sync wait && okdev exec -- python train.py`. It only waits; it does not start or repair sync.

Result collection via sync: add a second mapping with its own direction — no volume YAML needed (okdev auto-provisions the remote volume; the next `up` asks for `--reconcile`):

```yaml
sync:
  paths:
    - .:/workspace                 # code (bi or up)
    - local: ./collected           # may nest inside the repo
      remote: /data/results
      direction: down              # pod is the authority; local writes can't clobber results
```

Point the job's output at the mapping's remote root (`/data/results` above). This covers the hub pod; fleet-wide collection stays on `okdev jobs logs` / `okdev cp`.

## File Transfer

Use:

- `okdev cp ./local/path :/remote/path` for uploading files to a session pod
- `okdev cp :/remote/path ./local/path` for downloading files from a session pod
- `--all` or `--role` to fan out copies across multiple pods
- downloads from multiple pods are written into per-pod subdirectories
- single-file downloads resume automatically from a partial `<dest>` or `<dest>.okdev-part`; rerun `okdev cp` to continue, and add `--verify` to check SHA-256
- uploads are size-verified and atomic: the pod receives into a temp file, the byte count is checked, and the file is renamed into place — `okdev cp` exiting 0 means the complete file is on the pod, readers never observe a partial file, and dropped exec streams are retried automatically (no need to re-check with a follow-up `wc -c`)

## Teardown

Use:

- `okdev down`
- `okdev down --wait` when the user wants workload removal confirmed

## Temporary API outages before exec

Use an explicit session and `exec --preflight-retry-timeout 30s` when a caller
wants to tolerate a longer temporary API interruption during the read-only
session access check. Default zero retains two attempts. The budget respects
caller cancellation/deadlines, stops before command delivery, and does not
retry RBAC denials or a confirmed missing session. Config/session inference
and later target selection are outside this budget; `--timeout` is per-pod.

Preflight exit 78 has stderr diagnostics and empty stdout even with `--json`.
Once JSON envelopes exist, inspect every `status`, `exit` and `error`; process
exit zero alone is insufficient. Check reset/stop/detach results before the next
step, and retain the new detached job ID. A lost delivery response can hide a
successful launch: inspect launch-specific state rather than replaying it.
Commands, scripts and detach requests are not automatically replayed on stream
failure or remote nonzero exit. This is not an exactly-once guarantee. See
`docs/automation.md` for a checked reset/detach recipe.

## Readiness of a Newly Launched Detached Service

Use `jobs ready <job-id> --probe '<read-only command>' --timeout 2m` for health
plus launch identity. Retain the ID from a checked `exec --detach` result. The
probe runs in the job container and must exit zero with only that service's
inherited `OKDEV_JOB_ID` on stdout. An endpoint returning the old ID cannot pass;
a probe that echoes the expected ID without verifying the service defeats this
check. Adapt the service endpoint or use a service-written instance marker.

All selected discovered members must pass; terminal members and identity changes
fail early, and probes have an individual deadline (`--probe-timeout`, default
5s) inside the total wait. Probe failures/wrong IDs keep polling. Use bounded,
read-only probes (e.g. curl with `--max-time`); cancellation cannot promise that a
remote helper is killed. Success is point-in-time, and previously undiscovered
members are not inferred. JSON success contains jobId, ready and pods.

For replacement, stop only the exact prior job ID and intended pod(s), check the
exit, launch the replacement, keep its new ID, then run `jobs ready` before client
work. Do not add GPU reset as an implicit cleanup prerequisite on shared pods.
See `docs/automation.md` for the checked recipe. Keep `jobs wait` for completion
and `jobs wait --grep` for job-specific log milestones.

## Repeated Short Commands

Keep `okdev exec` for selectors, explicit containers and per-call session access
checks. There is no `exec --transport=ssh` flag. Local measurements show lower
latency for a reused SSH master, but that path skips some exec work and remains
bound to the original dev container; repinning the session does not retarget an
existing connection. Read `docs/exec-performance.md` for raw measurements,
reproduction and the remaining requirements for an integrated transport.

An SSH connection through Kubernetes port-forward still relies on the API stream.
A disconnected command may already have run: do not retry it automatically or
silently fall back to exec. Killing a local client is not proof that all remote
children stopped. Use tracked jobs with explicit stop operations when process
lifetime matters. Do not present `okdev ssh --cmd` as a stream-preserving exec
replacement; its helper combines output.

## Attach-only Existing Pods

Use `spec.attachOnly` with exactly one of `pods`, `selector`, or `session`, plus a
required `container`; omit workload declarations. Read `docs/attach-only.md` for
an example and prerequisites. Keep `--config` explicit: this mode does not save a
session association or target pin. Command selectors only narrow this scope.
Multi-pod exec/cp require explicit selection; jobs commands default to all scoped
pods. Existing owner labels and Kubernetes RBAC still apply.

Use exec/jobs/cp for access. Do not invoke up/down/restart, sync, managed SSH setup
or workload management through this config. No sidecar, tools, labels, host
aliases or lifecycle hooks are installed automatically. `attach-setup` runs the
configured setup each time and only on explicit invocation. It can mutate the
container, so match it to the user's requested setup, not ordinary inspection.

Detached jobs require shell/process tools and writable metadata storage. They are
tracked processes, not Kubernetes Jobs; arbitrary existing processes are not
imported. Pod replacement can lose metadata without persistent storage. Role
filters still require okdev labels: StatefulSet ordinals alone do not establish
worker roles. Use the external controller for workload lifecycle operations.
