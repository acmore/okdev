# Quickstart

## Prerequisites

- Go 1.21+
- `kubectl` configured to a cluster
- Namespace with permissions to create Pod/PVC

## Build

```bash
go build -o bin/okdev ./cmd/okdev
```

## Install prebuilt binary

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
okdev version
```

## Initialize config

```bash
./bin/okdev init
# or GPU/LLM-oriented template
./bin/okdev init --template gpu
```

Config discovery order:
1. `-c, --config <path>`
2. `.okdev.yaml`
3. `okdev.yaml`

## Start a session

```bash
./bin/okdev up
```

For a named session:

```bash
./bin/okdev up --session serving-main-alice
```

## Connect, sync, ports

```bash
./bin/okdev connect
./bin/okdev ssh --setup-key
./bin/okdev sync --mode up
./bin/okdev ports

# one-stop attach flow (shell + sync + app ports + ssh tunnel)
./bin/okdev up --attach

# continuous sync (syncthing)
./bin/okdev sync

# detached syncthing mode
./bin/okdev sync --background
```

`okdev` auto-installs local Syncthing and auto-injects a pod sidecar when `sync.engine=syncthing`.
Default sidecar image tag follows the running `okdev` binary version (`ghcr.io/<repo-owner>/okdev:<okdev-version>`). For dev builds, fallback is `ghcr.io/<repo-owner>/okdev:edge`. Update `spec.sync.syncthing.image` only if you publish to a different registry/repository.
Use `spec.sync.exclude` for local ignore patterns and `spec.sync.remoteExclude` for remote-only ignore patterns.
For SSH, default mode is `spec.ssh.mode=dev-container` (your dev image runs sshd). Set `spec.ssh.mode=sidecar` to use `ghcr.io/<repo-owner>/okdev-sshd:<okdev-version>` instead.
`okdev ssh` and `okdev up` manage `~/.ssh/config` entries as `okdev-<session>`.

Preview-only mode (no cluster changes):

```bash
./bin/okdev up --dry-run
./bin/okdev sync --dry-run
./bin/okdev down --dry-run
```

## Multi-session

```bash
./bin/okdev list
./bin/okdev use serving-main-alice
./bin/okdev status --all
# cross-owner visibility only when explicitly requested
./bin/okdev list --all-users
```

## Stop and cleanup

```bash
./bin/okdev down
./bin/okdev prune --ttl-hours 72
```
