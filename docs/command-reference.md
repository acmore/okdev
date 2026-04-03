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
- `okdev init [--workload pod|job|pytorchjob|generic] [--dev-image <image>] [--template basic|<registry>|<path>|<url>] [--stignore-preset default|python|node|go|rust] [--force]`
- `okdev validate`
- `okdev up [--wait-timeout 10m] [--dry-run]`
- `okdev down [session] [--delete-pvc] [--dry-run] [--output json]`
- `okdev status [session] [--all] [--all-users] [--details]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev target show`
- `okdev target set [--pod <name> | --role <role>]`
- `okdev agent list`
- `okdev exec [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev logs [session] [--container <name> | --all] [--tail N] [--since 5m] [--follow] [--previous]`
- `okdev ssh [session] [--setup-key] [--user root] [--cmd "..."] [--no-tmux] [--forward-agent|--no-forward-agent]`
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--foreground] [--reset] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
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

### `okdev init [--workload pod|job|pytorchjob|generic] [--template basic|<registry>|<path>|<url>] [--stignore-preset default|python|node|go|rust] [--force]`

- Writes a starter config manifest.
- Pod-only setups default to `.okdev.yaml`.
- When built-in workload scaffolding also writes files under `.okdev/`, the config is written as `.okdev/okdev.yaml`.
- `--workload`: chooses the scaffold mode. `pod` keeps the current simple config shape; `job` and `pytorchjob` add a `spec.workload` block plus a starter manifest; `generic` requires explicit inject information and can optionally use a preset such as `--generic-preset deployment`.
- `--dev-image`: sets the dev container image for pod workloads. When provided, the generated config includes a `podTemplate` with the given image.
- `--template`: accepts built-in `basic`, a user template name from `~/.okdev/templates/`, a file path, or a URL.
- For built-in templates, it also writes a starter local `.stignore` file for the initialized sync root.
- For built-in `basic`, `job` and `pytorchjob` scaffold `.okdev/job.yaml` or `.okdev/pytorchjob.yaml`. When the config itself is `.okdev/okdev.yaml`, the generated `manifestPath` stays relative to that directory, for example `job.yaml` or `pytorchjob.yaml`. okdev resolves those paths from `.okdev/` first and falls back to the project root for compatibility with older layouts. `generic --generic-preset deployment` scaffolds `.okdev/deployment.yaml` while still using `spec.workload.type=generic`.
- `--stignore-preset`: override the starter `.stignore` patterns with a project-oriented preset.
- When `--stignore-preset` is omitted, `okdev init` tries to detect a preset from common repo markers like `go.mod`, `package.json`, `Cargo.toml`, and `pyproject.toml`.

### `okdev up [--wait-timeout 10m] [--dry-run]`

- Reconciles Pod/PVC resources, updates SSH config, initializes managed forwarding/sync, then exits.
- If the session workload already exists, `okdev up` reuses it and only reruns setup.
- To force a fresh workload, run `okdev down` and then `okdev up`.
- tmux-backed persistent interactive shells are enabled by default.
- `--tmux`: explicitly enable tmux mode in the dev container.
- `--no-tmux`: disable tmux mode for this pod.
- When `sync.engine=syncthing`, `okdev up` refreshes the session's local Syncthing processes and starts background sync in bidirectional mode by default.
- `spec.ports` is materialized as SSH `LocalForward` or `RemoteForward` based on `direction`.

### `okdev down [session] [--delete-pvc] [--dry-run] [--output json]`

- Deletes the current session workload and cleans up local SSH/sync metadata.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Prompts for confirmation by default; use `--yes` in scripts or non-interactive environments.
- `--dry-run`: previews what would be deleted without removing cluster or local state.
- `--output json`: emits a machine-readable summary of the planned or completed deletion and local cleanup steps.
- `--delete-pvc` remains accepted for compatibility but is ignored; `okdev` no longer manages PVC lifecycle automatically.

### `okdev ssh [session] [--setup-key] [--user root] [--cmd "..."] [--no-tmux] [--forward-agent|--no-forward-agent]`

- Targets `okdev-sshd` in the `dev` container.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Maintains managed host alias in `~/.ssh/config` as `okdev-<session>`.
- `--no-tmux`: bypass tmux for this SSH session when tmux mode is enabled.
- `--forward-agent`: forwards the local `SSH_AUTH_SOCK` into the live SSH session so Git/SSH inside the workload can use your local agent.
- `--no-forward-agent`: disables forwarding for this SSH session even when `spec.ssh.forwardAgent: true`.
- Agent forwarding only applies to the live `okdev ssh` connection; keys are not copied into the workload.

### `okdev logs [session] [--container <name> | --all] [--tail N] [--since 5m] [--follow] [--previous]`

- Streams logs from the resolved target pod for the current session.
- When `session` is provided, `okdev` can resolve the saved config from session metadata even outside the repo.
- Defaults to the pinned target container when `--container` is omitted.
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
- `--reset`: stop the session's existing local sync processes and local Syncthing state, then bootstrap sync again.
- `okdev init` writes the starter config and, for built-in templates, a starter local `.stignore` file for the initialized sync root.

### `okdev upgrade`

- Checks the latest GitHub release and upgrades the `okdev` binary in place.
- Downloads the correct archive for the current OS/architecture, verifies the SHA256 checksum, and atomically replaces the running binary.
- No-op when already on the latest version.
- After `okdev up` completes successfully, a non-blocking version check runs (cached for 24 hours) and prints a reminder to stderr if a newer version is available.
