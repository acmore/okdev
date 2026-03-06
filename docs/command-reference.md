# Command Reference

## Global Flags

- `-c, --config`: config file path
- `--session`: session name override
- `-n, --namespace`: namespace override
- `--context`: kube context override
- `--output text|json`: output format for list/status

## Commands

- `okdev version`
- `okdev init [--template basic|gpu] [--force]`
- `okdev validate`
- `okdev up [--attach] [--wait-timeout 3m]`
- `okdev down [--delete-pvc]`
- `okdev status [--all]`
- `okdev list [--all-namespaces]`
- `okdev use <session>`
- `okdev connect [--shell /bin/bash] [--cmd "..."] [--no-tty]`
- `okdev ssh [--setup-key] [--user root] [--cmd "..."]`
- `okdev ports`
- `okdev sync [--mode up|down|bi] [--engine native|syncthing] [--watch]`
- `okdev prune [--ttl-hours 72] [--all-namespaces]`
