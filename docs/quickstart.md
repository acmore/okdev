# Quickstart

## Prerequisites

- Go 1.21+
- `kubectl` configured to a cluster
- Namespace with permissions to create Pod/PVC

## Build

```bash
go build -o bin/okdev ./cmd/okdev
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
./bin/okdev sync --mode up
./bin/okdev ports
```

## Multi-session

```bash
./bin/okdev list
./bin/okdev use serving-main-alice
./bin/okdev status --all
```

## Stop and cleanup

```bash
./bin/okdev down
./bin/okdev prune --ttl-hours 72
```
