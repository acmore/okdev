# Command Reference

## Global Flags

- `-c, --config`: config file path
- `--session`: session name override
- `--owner`: owner identity override (default: `OKDEV_OWNER` or local `USER`)
- `-n, --namespace`: namespace override
- `--context`: kube context override
- `--output text|json`: output format for list/status
- `--verbose`: enable debug logging

## Commands

- `okdev version`
- `okdev init [--template basic|gpu|llm-stack] [--force]`
- `okdev validate`
- `okdev up [--wait-timeout 3m] [--dry-run]`
  - prepares pod/workspace, SSH config, managed SSH+port-forwards, and background sync (when enabled), then exits
  - `spec.ports` are applied via managed SSH `LocalForward` rules
- `okdev down [--delete-pvc] [--dry-run]`
- `okdev status [--all] [--all-users]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."]`
  - SSH always targets the merged `okdev-sidecar` container (`sshd` + syncthing).
  - `okdev` writes managed host aliases to `~/.ssh/config` as `okdev-<session>`.
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--background] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
