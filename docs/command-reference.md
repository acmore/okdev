# Command Reference

## Global Flags

- `-c, --config`: configuration file path
- `--session`: explicit session identifier
- `--owner`: owner label override (default: `OKDEV_OWNER` or local `USER`)
- `-n, --namespace`: namespace override
- `--context`: kubeconfig context override
- `--output text|json`: output format (`list`, `status`)
- `--verbose`: debug logging

## Commands

- `okdev version`
- `okdev init [--template basic|gpu|llm-stack] [--force]`
- `okdev validate`
- `okdev up [--wait-timeout 3m] [--dry-run]`
  - Reconciles Pod/PVC resources, updates SSH config, initializes managed forwarding/sync, then exits.
  - `spec.ports` is materialized as SSH `LocalForward`.
- `okdev down [--delete-pvc] [--dry-run]`
- `okdev status [--all] [--all-users]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."]`
  - Targets merged `okdev-sidecar` (`sshd` + Syncthing).
  - Maintains managed host alias in `~/.ssh/config` as `okdev-<session>`.
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--background] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
