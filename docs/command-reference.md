# Command Reference

## Global Flags

- `-c, --config`: config file path
- `--session`: session name override
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
- `okdev status [--all]`
- `okdev list [--all-namespaces]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."]`
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--engine native|syncthing] [--watch] [--background] [--dry-run] [--force]`
- `okdev prune [--ttl-hours 72] [--all-namespaces] [--include-pvc] [--dry-run]`
