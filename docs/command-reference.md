# Command Reference

## Global Flags

- `-c, --config`: configuration file path
- `--session`: explicit session identifier
- `--owner`: owner label override (default: `OKDEV_OWNER` or local `USER`)
- `-n, --namespace`: namespace override
- `--context`: kubeconfig context override. When omitted, `spec.kubeContext` is used if set; otherwise kubeconfig current-context is used.
- `--output text|json`: output format (`list`, `status`)
- `--verbose`: debug logging
- `NO_COLOR=1` or `TERM=dumb`: disable ANSI color/styling in interactive terminal output

## Exit Codes

Commands that resolve a session distinguish failure classes so scripts and
agents can react without launching a diagnostic chain on every blip:

- `0`: success.
- non-zero from a remote command (e.g. `okdev exec -- false` → `1`, `exit 7` → `7`): the remote process's own status, preserved verbatim on single-pod runs. A multi-pod fanout where the command exited non-zero on some pods exits `1` (there is no single status to preserve).
- `74`: the cluster was reachable but the session has no pods — it is genuinely gone.
- `78`: a transient cluster-contact failure (API timeout, connection refused/reset, server overload). okdev already retries once with backoff before surfacing this; on `78` the caller should retry rather than treat the session as dead.
- `69`: a fanout `okdev exec` could not run the command on at least one pod (unreachable, timeout, container gone) — a delivery failure, as opposed to the command itself exiting non-zero, which stays exit `1`.
- `1`: any other error (configuration, permissions/RBAC, a failed job result, a fanout command that exited non-zero on some pods, etc.).

## Commands

- `okdev version`
- `okdev init [--workload pod|job|pytorchjob|generic] [--dev-image <image>] [--template <name>|<path>|<url>] [--set key=value] [--stignore-preset default|python|node|go|rust] [--force]`
- `okdev template list [--all]`
- `okdev template show <name>`
- `okdev validate`
- `okdev up [--wait-timeout 10m] [--dry-run]`
- `okdev down [session] [--delete-pvc] [--dry-run] [--wait] [--wait-timeout 2m] [--output json]`
- `okdev restart [session] [--yes] [--wait-timeout 10m]`
- `okdev status [session] [--all] [--all-users] [--details]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev target show`
- `okdev target set [--pod <name> | --role <role>]`
- `okdev agent list`
- `okdev exec [session] [--shell /bin/bash] [--no-tty] [--pod <name> | --role <role> | --label <k=v>] [--exclude <pod>] [--container <name>] [--detach] [--timeout <duration>] [--log-dir <path>] [--no-prefix] [--json] [--require-all] [--gateway <pod>] [--fanout N] [--pkill <pattern> [--signal <sig>]] [--require-sync] [-- command...]`
- `okdev jobs list [session] [--job-id <id>] [--container <name>] [--fanout N]`
- `okdev jobs logs <job-id> [session] [-f|--follow] [--tail N] [--since <dur|time>] [--pod <name> | --role <role> | --label <k=v>] [--exclude <pod>] [--container <name>] [--fanout N]`
- `okdev jobs stop <job-id> [session] [--container <name>] [--fanout N]`
- `okdev jobs wait <job-id> [session] [--container <name>] [--fanout N]`
- `okdev exec-jobs [session] [--job-id <id>] [--container <name>] [--fanout N]`
- `okdev cp [session] <src> <dst> [--all | --pod <name> | --role <role> | --label <k=v>] [--exclude <pod>] [--container <name>] [--fanout N]`
- `okdev logs [session] [--container <name> | --all] [--tail N] [--since 5m] [--follow] [--previous]`
- `okdev ssh [session] [--setup-key] [--user root] [--cmd "..."] [--no-tmux] [--forward-agent|--no-forward-agent]`
- `okdev ports`
- `okdev port-forward [session] <local:remote>... [--address <addr>[,<addr>...]] [--pod <name> | --role <role>] [--ready-only]`
- `okdev sync [--mode up|down|bi] [--foreground] [--reset] [--dry-run]`
- `okdev sync wait [session] [--timeout 10m]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
- `okdev migrate [--template <name>] [--set key=value] [--dry-run] [--yes]`
- `okdev upgrade`

### `okdev status [session] [--all] [--all-users] [--details]`

- Shows session status for the current or selected session.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- `--details`: prints a single-session diagnostic view with target selection, pod list, pod IP and node placement, mount persistence, sync path semantics, managed SSH state, key local paths, and target pod details.
- Abnormally terminated containers are surfaced per pod, e.g. `container pytorch: OOMKilled (exit 137, finished 2026-07-05T06:32:11Z)` — both the current state (container stayed down) and the last termination before a restart (suffixed `before last restart`). The pod `reason` column also falls back to the termination reason when no waiting reason applies.
- Detailed JSON output includes per-pod mount metadata, a `containerIssues` array (`container`, `reason`, `exitCode`, `finishedAt`, `current`), and a `pathSemantics` section describing which configured sync paths are shared via the workspace sync and which are expected to survive a session restart.
- `--details` is only valid when exactly one session is selected.

### `okdev target show`

- Prints the pinned interactive target for the current session and shows the session pod set.
- The selected row is marked with `*`.

### `okdev target set [--pod <name> | --role <role>]`

- Explicitly repins the interactive target used by `ssh`, `exec`, `ports`, and sync.
- `--pod` selects one concrete session pod.
- `--role` selects the highest-priority eligible pod with the matching `okdev.io/workload-role`.
- When attachable pods are defined, repinning is restricted to those pods.

### `okdev agent list`

- Shows configured coding agents, whether their CLI binary is installed, and whether auth is staged in the current session container.
- `okdev up` performs best-effort install checks for configured agents, bootstraps a modern Node/npm runtime via `nvm` when supported, and then installs missing CLIs.
- `okdev up` also stages local auth files for dedicated sessions when a configured local auth file exists.
- Codex uses `~/.codex/auth.json` by default, but `spec.agents[].auth.localPath` can point at a different local auth file.
- `okdev down` removes staged agent auth symlinks/runtime files when it can still reach the target container.
- `okdev` does not own agent process launch; users run `codex`, `claude`, `gemini`, `opencode`, or similar CLIs manually after connecting.

### `okdev exec [session] [-- command...]`

- Opens a shell or runs a command in one or more session pods.
- Without `-- command...`, opens an interactive shell on the pinned target pod.
- With `-- command...`, runs the command across all running pods by default, with output prefixed by short pod name on a TTY. When stdout is piped or redirected (scripts, CI, agents), the prefix is auto-suppressed so captured output is clean; pass `--no-prefix=false` to force it back on.
- `--all` explicitly targets all session pods. This matches the existing default fanout behavior when you pass a command with no selector.
- `--workers` targets only worker-role pods.
- `--pod` accepts the full pod name, the short name shown by `okdev status` (e.g. `master-0`, `worker-1`), or any unique `-<name>` suffix — no need to carry the full hash name across pod recreations. Unknown or ambiguous names error out (listing the available names) instead of silently matching nothing. `--exclude` accepts the same aliases (unknown exclusions are tolerated). The same aliases work for `okdev cp --pod`, `okdev jobs ... --pod`, and `okdev target set --pod`.
- `--group <pod-a,pod-b>` targets an explicit pod group. Repeat `--group` to define multiple groups, and use either full pod names or the short names shown in `okdev status`. A pod may appear in at most one group.
- Declared groups run one after another by default.
- `--sequential` runs selected pods one-by-one when no groups are declared (output and `--log-dir` layout are unchanged), or makes the default group-after-group ordering explicit when groups are declared.
- `--parallel` runs declared groups in parallel. `--fanout` caps total concurrent pod executions across all groups.
- `--script <file>` uploads a local script file and runs it across the targeted pods. In v1 this is local-file-only; `--script -` is not supported.
- With `--script`, everything after `--` is passed to the script as positional arguments (`$1`, `$2`, … / `"$@"`): `okdev exec --script ./bench.sh -- --steps 100 --warmup 10` runs `bench.sh` on each pod with `$1=--steps $2=100 $3=--warmup $4=10`. Arguments containing spaces are preserved.
- Failure framing for `-- command...` fanout: when every failing pod merely reported a non-zero exit code from the command itself, the summary is headed `COMMAND EXITED NON-ZERO:` and `okdev` exits with the remote status on a single-pod run (`exit 3` → `3`) or `1` on a multi-pod run — either way `okdev exec -- cmd && next` still short-circuits `&&` chains. When any pod could not run the command at all (unreachable, timeout, container gone), the summary is headed `FAILED:` and `okdev` exits `69`, letting scripts tell "my command failed" apart from "delivery failed" without parsing output.
- The `-- command...` path is non-interactive fanout execution, not a terminal session. TTY-dependent programs such as `watch`, `top`, `htop`, or full-screen TUIs are not expected to work there; use `okdev ssh` or `okdev exec` without `-- command...` to open an interactive shell first.
- `-- command...` preserves raw argv to the remote process. If you choose `bash -lc '...'`, that remote shell layer is still yours.
- `--script` avoids the quoting/stitching problem for non-trivial shell pipelines by uploading the file and running it remotely instead of embedding the body in a one-liner.
- `--pod`: target specific pods by name (repeatable or comma-separated).
- `--role`: target pods by `okdev.io/workload-role` label (case-insensitive).
- `--label`: target pods by arbitrary label `key=value` (repeatable, AND logic).
- `--exclude`: exclude specific pods from the selected set (repeatable or comma-separated). Cannot be used with `--pod`.
- `--group` is mutually exclusive with `--all`, `--workers`, `--pod`, `--role`, `--label`, and `--exclude`.
- `--parallel` and `--sequential` are mutually exclusive.
- `--container`: override which container to exec into (default: session target container). Works in both modes.
- `--script` executes the uploaded file directly when it has a shebang; otherwise it falls back to `sh <file> ...`.
- `--shell` cannot be combined with `--script`.
- `--detach`: launches the command in the background and returns immediately. Prints per-pod launch details including job id, pid, log path, and metadata path. The command runs in its own process group (via `setsid`) so `okdev jobs stop` can terminate the entire process tree, not just the leader.
- When `--log-dir` is used with multiple declared groups, logs are written into per-group subdirectories under the requested path. `--log-dir` has no effect with `--detach` (detached jobs log inside the pod).
- Detached exec preserves argv to the remote process; it is not flattened back into a single shell string. Detached `--script` uploads a per-pod temp file, launches it, and removes that temp file after completion.
- Detached jobs store combined stdout/stderr and JSON metadata under `/var/okdev/exec/` on the shared okdev-runtime volume, so logs and metadata survive a target-container crash (e.g. OOMKill) and remain readable through the sidecar. If that directory cannot be created or written by the container user (e.g. a user-supplied root-owned volume with a non-root container), the launcher falls back to the legacy `/tmp/okdev-exec/` with a notice on stderr — same lifetime semantics as before. For large outputs (training logs, profiles, dumps), prefer `--log-dir` on the caller or redirect inside your command to a session volume; the runtime volume is a node-local emptyDir and heavy writers can fill it.
- The `pid` returned from `--detach` is the pid of your command itself (okdev re-parents the launcher so `$!` is your program's pid after `execve`). `kill <pid>` or `okdev exec --pod <pod> -- kill <pid>` therefore targets your command, not a wrapper shell.
- When using `--detach`, pass the command you want okdev to launch directly. Do not add an extra `nohup ... &` inside the command string.
- `--timeout`: per-pod command timeout (e.g., `30s`, `5m`). Pods exceeding the timeout are cancelled and reported as failed.
- `--require-sync` (with `--detach`): refuse to launch unless background sync is healthy **and** every mapping has fully converged (an inline `okdev sync wait`). Without it, a detached launch on a dead or unhealthy sync channel still proceeds but prints a stderr warning that the job may run stale code. Requires the session to have sync mappings.
- `--pkill <pattern>`: kill processes whose full cmdline matches the extended regex, on the selected pods. Unlike a raw `pkill -f`, it can never match okdev's own exec machinery (the helper excludes itself and its whole ancestor chain), so no bracket trick (`pkill -f 'patter[n]'`) is needed. Prints one `killed <pid> <cmdline>` line per signalled process and follows the pkill exit convention: 0 when at least one process matched, 1 otherwise. `--signal <name|number>` (default `TERM`) selects the signal. Cannot be combined with a command after `--`, `--script`, `--shell`, or `--detach`. For detached jobs prefer `okdev jobs stop`, which terminates the whole process group.
- `--log-dir`: write per-pod output to `<dir>/<short-name>.log`. Streaming to stdout still happens.
- `--no-prefix`: suppress the pod name prefix in output. Auto-enabled when stdout is not a terminal (pipe/redirect/CI); pass `--no-prefix=false` to keep the prefix in that case.
- `--fanout N`: maximum concurrent pod executions (default 16).
- `--pod`, `--role`, and `--label` are mutually exclusive.
- `--json`: capture each pod's result into a JSON envelope instead of streaming. Output is always a JSON array with one envelope per pod in selection order — a single resolved pod still emits a length-1 array, so callers never branch on object-vs-array by pod count. Each envelope is `{"pod": "<name>", "exit": N, "stdout": "...", "stderr": "...", "status": "..."}`, with an `"error"` field added when the command could not be run or completed (in which case `exit` is `-1`).
  - `status` makes per-pod delivery explicit: `responded` (the command ran and reported an exit code — even a non-zero one), `unreachable` (the pod could not be reached; the command did not run), `timeout`, `error` (outcome unknown), or `missing` (no result came back for the pod). Use it to tell "responded with empty output" apart from "never responded".
  - `--require-all` (requires `--json`): exit non-zero after emitting the JSON document unless every targeted pod's status is `responded`, listing the missing pods. A responded pod with a non-zero exit code does not fail `--require-all` — its exit code is data.
  - With `spec.exec.fanoutMode: auto|gateway` and interpod SSH enabled, multi-pod `--json` runs route through an in-cluster gateway pod: one apiserver exec, fanout over the pod network (see `spec.exec` in the config manifest). `--gateway <pod>` picks the gateway explicitly (and enables the gateway path even below the auto threshold). If the gateway driver is unavailable (older sidecar image), okdev prints a notice on stderr and falls back to direct per-pod exec.
  - The remote command's own exit code is reported as data in `exit`, so a non-zero remote exit (e.g. `pgrep` returning 1 for no match) does **not** make `okdev` exit non-zero. `okdev` exits 0 whenever the JSON document is produced; inspect each envelope's `exit`/`error` to react. Pre-flight failures (session not found, cluster unreachable) still use stderr and the dedicated exit codes (74/78) rather than JSON.
  - `--json` requires a command (after `--` or via `--script`) and cannot be combined with `--detach`, `--group`, `--parallel`, `--sequential`, or `--log-dir`. The `[<pod>]` prefix is never applied in JSON mode.

### `okdev jobs list [session]`

- Lists detached `okdev exec --detach` jobs across the session's running pods.
- Reads job metadata from `/var/okdev/exec/*.json` in the target container (plus the legacy `/tmp/okdev-exec/` location for jobs launched by older okdev versions).
- If the target container is gone (e.g. OOMKilled), listing and `jobs logs` automatically retry through the `okdev-sidecar` container, which mounts the same runtime volume.
- When a command fails because the container is gone, the error carries the termination cause when it can be determined, e.g. `container "pytorch" terminated: OOMKilled (exit 137, finished 2026-07-05T06:32:11Z)`. The same hint is appended to `okdev exec` FAILED lines and single-pod exec errors.
- Text output is grouped by logical job id, with one row per detached launch showing job id, summarized state, pod count, earliest start time, and original command.
- On a terminal the COMMAND column is truncated (with `…`) to the remaining terminal width so long training commands don't wrap rows; piped/redirected output and `--json` always carry the full command.
- JSON output includes both the logical job summary and the per-pod `podStates` records (with `pgid` and `groupLive` — the count of live processes still in the job's process group — when available).
- State values: `running` (wrapper alive and user command in flight), `exited` (user command finished; exit code is recorded in `podStates` / JSON), and `orphaned` (metadata still says `running` but the pid has exited or been recycled - typically the wrapper was `SIGKILL`ed or the container was restarted before the completion metadata could be written). Grouped text summaries render forms such as `running(1/2)`, `exited(2/2)`, and `failed(1/2)`.
- When any pod fails to list its jobs, the command still prints the jobs it was able to collect from the healthy pods and reports the failures in a `FAILED:` footer (or an `errors` array in `--json` output); exit status is non-zero so scripts can detect partial failures.
- With `spec.exec.fanoutMode: auto|gateway` and interpod SSH enabled, the per-pod listing routes through the in-cluster fanout gateway (one apiserver exec instead of one per pod). The listing is read-only, so any gateway problem silently falls back to direct per-pod queries.
- `--pod`: target specific pods by name (repeatable or comma-separated).
- `--role`: target pods by `okdev.io/workload-role` label (case-insensitive).
- `--label`: target pods by arbitrary label `key=value` (repeatable, AND logic).
- `--exclude`: exclude specific pods from the selected set (repeatable or comma-separated). Cannot be used with `--pod`.
- `--container`: override which container to inspect (default: session target container).
- `--job-id`: filter to a specific detached exec job id.
- `--fanout N`: maximum concurrent pod queries (default 16).
- `--ready-only`: inspect only pods that are already running.

### `okdev jobs logs <job-id> [session]`

- Streams the detached job's combined stdout/stderr aggregated across all pods in the logical job.
- Each line is prefixed by the pod short name so multi-pod output stays attributable.
- `--pod`: stream logs from specific pods by name (repeatable or comma-separated).
- `--role`: stream logs from pods with the matching workload role.
- `--label`: stream logs from pods matching label selectors.
- `--exclude`: exclude specific pods from the selected set. Cannot be used with `--pod`.
- `-f` / `--follow`: keep following until every pod in the job reaches a terminal state.
- `--tail N`: show only the last N lines of each pod's log (`-1` = all, the default). Combines with `--follow` (`tail -n N -f`).
- `--since <dur|RFC3339>`: skip pods whose log **file** has not changed since the cutoff (e.g. `--since 90s` in a poll loop transfers nothing from idle pods). Job logs carry no per-line timestamps, so this is a file-level activity gate — when a file has changed, the (tail-limited) current content is shown, not just the lines after the cutoff. Cannot be combined with `--follow`.
- Reads are size-verified: the reader reports the exact byte count of the selection and okdev retries when the received stream is truncated or empty-by-drop, so `jobs logs` never silently presents a dropped exec stream as the job's output.
- If the job's container is gone (e.g. OOMKilled), logs are read through the `okdev-sidecar` container instead — job logs live on the shared runtime volume and survive the container.
- If some pod logs are unavailable, okdev still streams the logs it can read and reports the missing pods in a `FAILED:` footer before returning non-zero.

### `okdev jobs wait <job-id> [session]`

- Polls the detached job until all pod-local records are terminal.
- Returns success only when every pod exits cleanly.
- Returns non-zero when any pod exits non-zero, becomes `orphaned`, cannot be queried, or the job id does not exist.

### `okdev jobs stop <job-id> [session]`

- Stops every still-running pod in the logical job by sending `SIGTERM`, waiting 10 seconds, then sending `SIGKILL` to survivors.
- Signals the job's whole **process group** when available: detached jobs are launched via `setsid`, so children the command forked (e.g. `torchrun` workers) are signaled too — including stragglers whose leader already exited. Stop only reports success once no live group member remains, so a clean return means the process tree (and its GPU memory) is gone.
- Group membership is verified via the job's `OKDEV_JOB_ID` environment marker before signaling, so recycled pids/pgids are never signaled by mistake. Jobs launched by older okdev versions (or in containers without `setsid`) fall back to leader-only signaling.
- Prints which pods were signaled (`pgid=N` for group signals, `pid=N` for the fallback) during the stop flow.
- Returns non-zero when any pod could not be queried or signaled, or if processes are still alive after the stop attempt.

### `okdev exec-jobs [session]`

- Compatibility alias for `okdev jobs list [session]`.
- Uses the same grouped logical-job output and flags as `okdev jobs list`.

### `okdev cp [session] <src> <dst>`

- Copies files or directories between the local machine and session pods.
- Prefix the remote path with `:` (e.g., `:/workspace/data`). The other argument is a local path.
- **Single-pod mode** (default): copies to/from the pinned target pod.
- **Multi-pod upload** (`--all`, `--pod`, `--role`, `--label`): fans out the same local source to all matched pods in parallel.
- **Multi-pod download**: downloads from each matched pod into `<dest>/<short-pod-name>/` subdirectories.
- Files are streamed via `cat` pipes. Directories are tar-streamed automatically.
- Single-file downloads resume from an undersized `<dest>` or a saved `<dest>.okdev-part` from a previous attempt.
- If a previous attempt finished streaming but failed before the final rename, rerunning `okdev cp` promotes the completed `<dest>.okdev-part` without redownloading.
- `--verify` verifies single-file download SHA-256 after copy. It is not supported for uploads or directory downloads.
- With `--verify`, resumed downloads hash the existing local bytes once and continue hashing the streamed remainder, so success does not require a second full local reread.
- `--verify` currently requires `python3` or `python` in the target container to emit the remote SHA-256 during the download stream.
- Directory and multi-pod downloads do not yet support resume.
- On a TTY, an in-place progress line shows transferred bytes, transfer rate, and elapsed time (after a few seconds). For multi-pod copies the line aggregates across pods (e.g. `Copying to 8 pods · 3/8 done · 5 in flight · 1.2 GiB · 28.0 MiB/s · 00:42`) and surfaces a noticeably slow pod inline. Progress is suppressed on non-TTY writers (pipes, redirects, CI), so machine-readable output is unaffected.
- `--all`: target all running pods in the session.
- `--pod`: target specific pods by name (repeatable or comma-separated).
- `--role`: target pods by `okdev.io/workload-role` label (case-insensitive).
- `--label`: target pods by arbitrary label `key=value` (repeatable, AND logic).
- `--exclude`: exclude specific pods from the selected set (repeatable or comma-separated). Cannot be used with `--pod`.
- `--container`: override which container to copy to/from (default: session target container).
- `--fanout N`: maximum concurrent pod transfers (default 16).
- `--verify`: verify single-file download SHA-256 after copy.
- `--all`, `--pod`, `--role`, and `--label` are mutually exclusive.

### `okdev init [--workload pod|job|pytorchjob|generic] [--template <name>|<path>|<url>] [--set key=value] [--stignore-preset default|python|node|go|rust] [--force]`

- Writes a starter config manifest.
- Pod-only setups default to `.okdev.yaml`.
- When built-in workload scaffolding also writes files under `.okdev/`, the config is written as `.okdev/okdev.yaml`.
- `--workload`: chooses the scaffold mode. `pod` keeps the current simple config shape; `job` and `pytorchjob` add a `spec.workload` block plus a starter manifest; `generic` requires explicit inject information and can optionally use a preset such as `--generic-preset deployment`.
- `--dev-image`: sets the dev container image for pod workloads. Pod configs generated from the built-in template include a `podTemplate` with the dev container image plus equal CPU/memory requests and limits for the dev container and sidecar, so the starter pod is eligible for Kubernetes `Guaranteed` QoS.
- `--template`: accepts a project template from `.okdev/templates/<name>.yaml.tmpl`, a user template from `~/.okdev/templates/<name>.yaml.tmpl`, built-in `basic`, a file path, or a URL. Run `okdev template list` to see available names.
- `--set`: sets a frontmatter-declared template variable. Repeat it for multiple variables, for example `--set numWorkers=4 --set baseImage=pytorch:latest`.
- Templates can declare `string`, `int`, and `bool` variables in YAML frontmatter. Resolved values are available as `.Vars.<name>` during rendering and are persisted under `spec.template.vars`.
- Templates can also declare companion `files` in frontmatter. Each file has a rendered `path` and a `template` path resolved relative to the selected template, which lets a PyTorch template render both `okdev.yaml` and a matching `pytorchjob.yaml`.
- For built-in templates, it also writes a starter local `.stignore` file for the initialized sync root.
- For built-in `basic`, `job` and `pytorchjob` scaffold `.okdev/job.yaml` or `.okdev/pytorchjob.yaml`. When the config itself is `.okdev/okdev.yaml`, the generated `manifestPath` stays relative to that directory, for example `job.yaml` or `pytorchjob.yaml`. okdev resolves those paths from `.okdev/` first and falls back to the project root for compatibility with older layouts. `generic --generic-preset deployment` scaffolds `.okdev/deployment.yaml` while still using `spec.workload.type=generic`.
- `--stignore-preset`: override the starter `.stignore` patterns with a project-oriented preset.
- When `--stignore-preset` is omitted, `okdev init` tries to detect a preset from common repo markers like `go.mod`, `package.json`, `Cargo.toml`, and `pyproject.toml`.

### `okdev template list [--all]`

- Lists templates from project, user, and built-in sources in resolution order.
- Project templates in `.okdev/templates/` shadow user templates, and user templates shadow built-ins.
- `--all` includes shadowed lower-priority entries.

### `okdev template show <name>`

- Prints a template's resolved source, description, declared companion files, and declared variables.
- Description and variables come from optional YAML frontmatter at the top of the template.

### `okdev migrate [--template <name>] [--set key=value] [--dry-run] [--yes]`

- Without `--template`, migrates older config schema fields to the current schema.
- With `--template`, re-renders the selected template, overlays existing config values so local edits win, writes the merged config, and regenerates any template-declared companion files. Unlike the config, companion files are fully overwritten from the re-rendered template; local edits to those files will be lost (the `.bak` backup is your recovery path).
- Existing `spec.template.vars` seed template variables during migration; `--set` overrides stored values.
- Unless `--no-backup` is set, `okdev migrate --template` writes `.bak` backups for the config and any rewritten companion files before overwriting them.
- `--dry-run` prints the merged config and lists the companion files that would be rewritten, without writing. `--yes` skips confirmation when writing.

### `okdev up [--wait-timeout 10m] [--dry-run]`

- Reconciles Pod/PVC resources, updates SSH config, initializes managed forwarding/sync, then exits.
- If the session workload already exists, `okdev up` reuses it and only reruns setup.
- Workload resources are named per run, for example `okdev-<session>-<run-id>`; `okdev down && okdev up` creates a fresh workload name for the same session.
- If a Job or PyTorchJob created or recreated by this `okdev up` reaches a failed pod state before readiness, okdev stops waiting, deletes that failed workload, and clears local session state. Reused existing workloads are not auto-deleted.
- tmux-backed persistent interactive shells are enabled by default.
- `--tmux`: explicitly enable tmux mode in the dev container.
- `--no-tmux`: disable tmux mode for this pod.
- When `sync.engine=syncthing`, `okdev up` refreshes the session's local Syncthing processes, starts background sync in bidirectional mode by default, and waits for the initial sync to converge before exiting.
- `spec.ports` is materialized as SSH `LocalForward` or `RemoteForward` based on `direction`.

### `okdev down [session] [--delete-pvc] [--dry-run] [--wait] [--wait-timeout 2m] [--output json]`

- Deletes the current session workload and cleans up local SSH/sync metadata.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Prompts for confirmation by default; use `--yes` in scripts or non-interactive environments.
- `--dry-run`: previews what would be deleted without removing cluster or local state.
- `--wait`: waits for the workload object to disappear and then for any remaining session pods to terminate before returning.
- `--wait-timeout`: caps the total time spent waiting for workload/pod termination when `--wait` is enabled.
- `--output json`: emits a machine-readable summary of the planned or completed deletion and local cleanup steps.
- `--delete-pvc` remains accepted for compatibility but is ignored; `okdev` no longer manages PVC lifecycle automatically.

### `okdev restart [session] [--yes] [--wait-timeout 10m]`

- One-command recovery when a container died and the restart policy will not bring it back: deletes the session workload, waits for termination, resets local per-session sync state, and runs the full `up` flow against the current config.
- PVCs and other resources referenced by `spec.volumes` are untouched; auto-provisioned sync volumes are recreated.
- Lifecycle hooks (`postCreate`/`postSync`) re-run automatically on the fresh pods — configure them for setup that must survive recreation (installed tools, editable installs).
- Pod names change on recreation; use the short-name aliases (`master-0`, `worker-1`) with `--pod` so scripts survive restarts.
- Prompts for confirmation; `--yes` for scripts. `--wait-timeout` caps both the deletion wait and pod readiness.

### `okdev ssh [session] [--setup-key] [--user root] [--cmd "..."] [--no-tmux] [--forward-agent|--no-forward-agent]`

- Targets `okdev-sshd` in the resolved interactive container.
- The default interactive container name is `dev`; override it with `spec.workload.attach.container` when your workload uses a different main container name.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Maintains managed host alias in `~/.ssh/config` as `okdev-<session>`.
- `--no-tmux`: bypass tmux for this SSH session when tmux mode is enabled.
- `--forward-agent`: forwards the local `SSH_AUTH_SOCK` into the live SSH session so Git/SSH inside the workload can use your local agent.
- `--no-forward-agent`: disables forwarding for this SSH session even when `spec.ssh.forwardAgent: true`.
- Agent forwarding only applies to the live `okdev ssh` connection; keys are not copied into the workload.
- Requires a local SSH agent with identities already loaded, for example `eval "$(ssh-agent -s)"` and `ssh-add ~/.ssh/id_ed25519`.

### `okdev logs [session] [--container <name> | --all] [--tail N] [--since 5m] [--follow] [--previous]`

- Streams logs from the resolved target pod for the current session.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Defaults to the pinned target container when `--container` is omitted.
- The default target container name is `dev` unless `spec.workload.attach.container` selects a different interactive container.
- `--all`: streams all regular containers in the target pod and prefixes each line with `[container]`.
- `--follow`: follows logs by default; use `--follow=false` for a bounded dump.
- `--tail` and `--since` mirror the usual Kubernetes log filters.
- `--previous`: reads the previous instance logs for the selected container(s).

### `okdev ports`

- Advanced/recovery command. Rebuilds managed SSH `LocalForward` / `RemoteForward` state from `spec.ports` after disconnects or local port changes.
- No-op when managed forwards are already healthy and config is unchanged.
- `--dry-run`: previews the SSH alias and port-forward actions without updating SSH config or starting/stopping managed forwards.

### `okdev port-forward [session] <local:remote>...`

- Runs direct foreground Kubernetes port-forwarding to one selected session pod.
- Uses the current session target pod by default.
- `--address`: local listen addresses (default: `localhost`). Accepts comma-separated values or repeated flags, for example `--address localhost,0.0.0.0`.
- `--pod` selects one explicit pod by name.
- `--role` selects one pod by workload role. Ambiguity is an error.
- `--ready-only`: restricts selection to already-running pods.
- Only `LOCAL:REMOTE` mappings are supported (kubectl-style `:REMOTE` or bare `PORT` are rejected).
- The command stays attached until interrupted.

### `okdev sync [--mode up|down|bi] [--foreground] [--reset] [--dry-run]`

- Advanced command. Starts detached background sync by default; use `--foreground` for sync debugging, or explicit one-way sync (`up`/`down`).
- Scope: `okdev sync` manages the **channel** (start/repair the transport). A successful return does not mean pending changes have finished propagating — to guarantee your latest edits are on the pod, use `okdev sync wait` (the **data** question). The two are complementary, not interchangeable.
- By default each configured mapping syncs in its own `direction` (`spec.sync.paths[].direction`, falling back to `bi`); an explicit `--mode` forces that mode onto every mapping for the invocation. See the config manifest for choosing persistent directions — `down` makes the pod the authority so local writes can never clobber pod-generated results.
- With multiple mappings, each becomes its own syncthing folder; the first (primary) mapping is the one shared to mesh receivers, and `sync reset-remote` clears only the primary remote.
- A mapping's local root may nest inside the primary root: okdev maintains a managed block in the primary root's `.stignore` excluding it from the primary folder (written before the sync daemon starts). Removing a mapping retains the entry as a tombstone so the subtree never silently joins the primary folder; sync start prints a notice and `status --details` lists active/retained excludes.
- Without an explicit `--mode`, no-op when background sync is already active for the session.
- Liveness is judged by health, not just process existence: a sync process that is alive but unhealthy (e.g. peer disconnected after a network drop) is never reported as "already running" — `okdev sync` repairs it in place (reset local state + restart), so a plain `okdev sync` recovers a dead or stale channel without `--reset`. `okdev sync` and `okdev sync wait` share this health check and can no longer contradict each other about the same channel.
- `--background`: explicitly request detached background mode.
- `--reset`: check local-to-hub sync and mesh receiver health, then reset only what is broken. Skips the local sync teardown when the primary sync is already healthy. For sessions with mesh receivers, probes each receiver and re-runs mesh setup only when broken or disconnected receivers are found.
- `--reset --force` / `--reset -f`: unconditionally reset without health checks.
- `--reset --local`: scope reset to local-to-hub sync only (skip mesh).
- `--reset --mesh`: scope reset to mesh receivers only (skip local sync).
- `--reset --force --local` / `--reset --force --mesh`: force reset a specific component.
- `okdev init` writes the starter config and, for built-in templates, a starter local `.stignore` file for the initialized sync root.

### `okdev sync wait [session] [--timeout 10m]`

- Scope: the **data** question — has everything propagated? — complementary to `okdev sync`, which manages the channel. Blocks until every configured sync mapping has zero pending bytes in **both** directions, then returns — the edit-run loop guarantee: `vim train.py && okdev sync wait && okdev exec -- python train.py`.
- Triggers an immediate rescan on both sides before waiting, so files written moments earlier are picked up now instead of after the filesystem-watcher delay.
- Purely a wait: it does not start or repair sync. It fails fast with a state-accurate report: "not running" when the background process is gone (start it with `okdev sync`), or "running but unhealthy (…)" when the process is alive but the channel is broken (repair with `okdev sync`, which self-heals, or `okdev sync --reset`).
- Prints pending-byte progress while waiting; exits non-zero if convergence is not reached within `--timeout`.

### `okdev upgrade`

- Checks the latest GitHub release and upgrades the `okdev` binary in place.
- Downloads the correct archive for the current OS/architecture, verifies the SHA256 checksum, and atomically replaces the running binary.
- No-op when already on the latest version.
- After `okdev up` completes successfully, a non-blocking version check runs (cached for 24 hours) and prints a reminder to stderr if a newer version is available.
