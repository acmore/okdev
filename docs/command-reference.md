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
- `okdev up [--no-attach] [--wait-timeout 3m] [--dry-run]`
  - attach is enabled by default; use `--no-attach` to skip shell + background integrations
- `okdev down [--delete-pvc] [--dry-run]`
- `okdev status [--all] [--all-users]`
- `okdev list [--all-namespaces] [--all-users]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."]`
  - SSH target is configured by `spec.ssh.mode` (`dev-container` or `sidecar`).
  - `okdev` writes managed host aliases to `~/.ssh/config` as `okdev-<session>`.
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--background] [--dry-run]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--all-users] [--include-pvc] [--dry-run]`
