# Command Reference

## Global Flags

- `-c, --config`: configuration file path
- `--session`: explicit session identifier
- `--owner`: owner label override (default: `OKDEV_OWNER` or local `USER`)
- `-n, --namespace`: namespace override
- `--context`: kubeconfig context override. When omitted, `spec.kubeContext` is used if set; otherwise kubeconfig current-context is used.
- `--output text|json`: output format (`list`, `status`)
- `--verbose`: debug logging

## Commands

- `okdev version`
- `okdev init [--template basic|gpu|llm-stack] [--force]`
- `okdev validate`
- `okdev up [--wait-timeout 3m] [--dry-run]`
- `okdev down [--delete-pvc] [--dry-run]`
- `okdev status [--all] [--all-users]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."] [--no-tmux]`
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--background] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`

### `okdev up [--wait-timeout 3m] [--dry-run]`

- Reconciles Pod/PVC resources, updates SSH config, initializes managed forwarding/sync, then exits.
- tmux-backed persistent interactive shells are enabled by default.
- `--tmux`: explicitly enable tmux mode in the dev container.
- `--no-tmux`: disable tmux mode for this pod.
- When `sync.engine=syncthing`, `okdev up` starts background sync in bidirectional mode by default.
- `spec.ports` is materialized as SSH `LocalForward`.

### `okdev ssh [--setup-key] [--user root] [--cmd "..."] [--no-tmux]`

- Targets `okdev-sshd` in the `dev` container.
- Maintains managed host alias in `~/.ssh/config` as `okdev-<session>`.
- `--no-tmux`: bypass tmux for this SSH session when tmux mode is enabled.

### `okdev ports`

- Advanced/recovery command. Rebuilds managed SSH `LocalForward` state from `spec.ports` after disconnects or local port changes.
- No-op when managed forwards are already healthy and config is unchanged.

### `okdev sync [--mode up|down|bi] [--background] [--dry-run]`

- Advanced command. Use for foreground sync debugging, or explicit one-way sync (`up`/`down`).
- For default `--mode bi`, no-op when background sync is already active for the session.
