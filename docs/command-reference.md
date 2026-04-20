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

## Commands

- `okdev version`
- `okdev init [--workload pod|job|pytorchjob|generic] [--dev-image <image>] [--template <name>|<path>|<url>] [--set key=value] [--stignore-preset default|python|node|go|rust] [--force]`
- `okdev template list [--all]`
- `okdev template show <name>`
- `okdev validate`
- `okdev up [--wait-timeout 10m] [--dry-run]`
- `okdev down [session] [--delete-pvc] [--dry-run] [--wait] [--wait-timeout 2m] [--output json]`
- `okdev status [session] [--all] [--all-users] [--details]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev target show`
- `okdev target set [--pod <name> | --role <role>]`
- `okdev agent list`
- `okdev exec [session] [--shell /bin/bash] [--no-tty] [--pod <name> | --role <role> | --label <k=v>] [--exclude <pod>] [--container <name>] [--detach] [--timeout <duration>] [--log-dir <path>] [--no-prefix] [--fanout N] [-- command...]`
- `okdev cp [session] <src> <dst> [--all | --pod <name> | --role <role> | --label <k=v>] [--exclude <pod>] [--container <name>] [--fanout N]`
- `okdev logs [session] [--container <name> | --all] [--tail N] [--since 5m] [--follow] [--previous]`
- `okdev ssh [session] [--setup-key] [--user root] [--cmd "..."] [--no-tmux] [--forward-agent|--no-forward-agent]`
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--foreground] [--reset] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
- `okdev migrate [--template <name>] [--set key=value] [--dry-run] [--yes]`
- `okdev upgrade`

### `okdev status [session] [--all] [--all-users] [--details]`

- Shows session status for the current or selected session.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- `--details`: prints a single-session diagnostic view with target selection, pod list, managed SSH state, sync state, key local paths, and target pod details.
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
- With `-- command...`, runs the command across all running pods by default, with output prefixed by short pod name.
- `--pod`: target specific pods by name (repeatable or comma-separated).
- `--role`: target pods by `okdev.io/workload-role` label (case-insensitive).
- `--label`: target pods by arbitrary label `key=value` (repeatable, AND logic).
- `--exclude`: exclude specific pods from the selected set (repeatable or comma-separated). Cannot be used with `--pod`.
- `--container`: override which container to exec into (default: session target container). Works in both modes.
- `--detach`: wrap the command in `nohup` and return immediately. Prints per-pod confirmation.
- `--timeout`: per-pod command timeout (e.g., `30s`, `5m`). Pods exceeding the timeout are cancelled and reported as failed.
- `--log-dir`: write per-pod output to `<dir>/<short-name>.log`. Streaming to stdout still happens.
- `--no-prefix`: suppress the pod name prefix in output. Useful when targeting a single pod or piping.
- `--fanout N`: maximum concurrent pod executions (default 16).
- `--pod`, `--role`, and `--label` are mutually exclusive.

### `okdev cp [session] <src> <dst>`

- Copies files or directories between the local machine and session pods.
- Prefix the remote path with `:` (e.g., `:/workspace/data`). The other argument is a local path.
- **Single-pod mode** (default): copies to/from the pinned target pod.
- **Multi-pod upload** (`--all`, `--pod`, `--role`, `--label`): fans out the same local source to all matched pods in parallel.
- **Multi-pod download**: downloads from each matched pod into `<dest>/<short-pod-name>/` subdirectories.
- Files are streamed via `cat` pipes. Directories are tar-streamed automatically.
- Downloads extract files directly to the destination path as they arrive (no full-archive buffering), so intermediate files become visible on disk during the transfer.
- Before the transfer begins, an announce line on stderr reports the planned operation with total size when known (e.g. `Downloading :/workspace/data -> ./out (4.32 GB) via sess-master-0`).
- Directory copies parallelize by default (`--parallel 4`): the source file list is balanced across N concurrent `tar` exec streams per pod so a single slow SPDY connection is no longer the bottleneck. Empty directories inside the tree are not preserved by the parallel path.
- A throttled single-line progress indicator is rendered to stderr on TTYs showing bytes transferred, total size + percentage (when known), rate, ETA, file count, and current file. On non-TTY output the progress is silent and a final summary line is printed to stdout.
- `--all`: target all running pods in the session.
- `--pod`: target specific pods by name (repeatable or comma-separated).
- `--role`: target pods by `okdev.io/workload-role` label (case-insensitive).
- `--label`: target pods by arbitrary label `key=value` (repeatable, AND logic).
- `--exclude`: exclude specific pods from the selected set (repeatable or comma-separated). Cannot be used with `--pod`.
- `--container`: override which container to copy to/from (default: session target container).
- `--fanout N`: maximum concurrent pod transfers (default 16).
- `--parallel N`: parallel streams per pod for directory copies (default 4, max 16). Splits the file list into roughly size-balanced buckets and issues one `tar` exec stream per bucket; single-file copies ignore this flag. In multi-pod mode the effective per-pod parallelism is clamped so total streams (`fanout × parallel`) stays under a safe cap.
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
- With `--template`, re-renders the selected template, overlays existing config values so local edits win, and writes the merged config.
- Existing `spec.template.vars` seed template variables during migration; `--set` overrides stored values.
- `--dry-run` prints the merged config without writing. `--yes` skips confirmation when writing.

### `okdev up [--wait-timeout 10m] [--dry-run]`

- Reconciles Pod/PVC resources, updates SSH config, initializes managed forwarding/sync, then exits.
- If the session workload already exists, `okdev up` reuses it and only reruns setup.
- Workload resources are named per run, for example `okdev-<session>-<run-id>`; `okdev down && okdev up` creates a fresh workload name for the same session.
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

### `okdev sync [--mode up|down|bi] [--foreground] [--reset] [--dry-run]`

- Advanced command. Starts detached background sync by default; use `--foreground` for sync debugging, or explicit one-way sync (`up`/`down`).
- For default `--mode bi`, no-op when background sync is already active for the session.
- `--background`: explicitly request detached background mode.
- `--reset`: check local-to-hub sync and mesh receiver health, then reset only what is broken. Skips the local sync teardown when the primary sync is already healthy. For sessions with mesh receivers, probes each receiver and re-runs mesh setup only when broken or disconnected receivers are found.
- `--reset --force` / `--reset -f`: unconditionally reset without health checks.
- `--reset --local`: scope reset to local-to-hub sync only (skip mesh).
- `--reset --mesh`: scope reset to mesh receivers only (skip local sync).
- `--reset --force --local` / `--reset --force --mesh`: force reset a specific component.
- `okdev init` writes the starter config and, for built-in templates, a starter local `.stignore` file for the initialized sync root.

### `okdev upgrade`

- Checks the latest GitHub release and upgrades the `okdev` binary in place.
- Downloads the correct archive for the current OS/architecture, verifies the SHA256 checksum, and atomically replaces the running binary.
- No-op when already on the latest version.
- After `okdev up` completes successfully, a non-blocking version check runs (cached for 24 hours) and prints a reminder to stderr if a newer version is available.
