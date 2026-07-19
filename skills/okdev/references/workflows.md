# okdev Workflows

## First Look

Start by identifying:

- which config file is in play
- whether the workload is `pod` or manifest-backed (`job`, `generic`, `pytorchjob`)
- whether the user is trying to create a new session, reuse an existing one, or debug an existing one

Useful docs:

- `docs/quickstart.md`
- `docs/command-reference.md`
- `docs/config-manifest.md`

## Common Lifecycle

Typical flow:

1. `okdev init`
2. edit config or generated manifest if needed
3. `okdev up`
4. `okdev status --details`
5. connect with `okdev ssh` or `okdev exec`
6. `okdev down`

## Config Location

- Simple pod setups usually use `.okdev.yaml`.
- Manifest-backed workloads initialized by `okdev init` usually use `.okdev/okdev.yaml`.
- Generated companion manifests usually live beside that config under `.okdev/`.

If config-path confusion is possible, recommend `--config <path>` explicitly.

## Session Start

Use `okdev up` for the standard startup path.

Important behavior:

- `okdev up` creates or reuses the session workload
- starts `okdev-sshd`
- starts background Syncthing sync
- configures managed SSH host and port forwards

If the user expects workload-shaping changes to apply, check whether they need:

- `okdev up --reconcile`
- or `okdev down && okdev up` for a fresh workload

## Session Access

Use:

- `okdev ssh` for the managed interactive shell (bash or zsh; okdev tmux prefix is `ctrl-a`)
- `ssh okdev-<session>` for plain SSH
- `okdev exec` for multi-pod parallel command execution (pdsh-style fanout with `--role`/`--label`/`--pod`/`--group`/`--workers` targeting, `--script` to upload+run a file, `--detach` for background runs surfaced by `okdev jobs list`, `--pkill '<pattern>'` to kill matching processes without the raw-pkill self-match footgun, `--json` for structured per-pod results, and `--fanout` concurrency control)
- `okdev jobs logs <job-id>` accepts the same `--pod`/`--role`/`--label`/`--exclude` targeting as `okdev exec`; use it to tail a subset of a detached job's pods instead of every pod in the session

If attachable pods are involved, explain that interactive targeting follows attachable-pod selection rules rather than arbitrary pod choice.

## Sync

Default guidance:

- `okdev up` already starts sync and waits for initial convergence
- `.git` is synced by default
- `okdev status --details` shows whether sync is active
- `okdev sync --foreground` is useful when the user needs live troubleshooting
- `okdev sync --reset` is the standard repair path for stale local sync state
- `okdev sync wait` blocks until every mapping has zero pending bytes both ways — use it between an edit and a run to guarantee the pod sees the latest files: `vim train.py && okdev sync wait && okdev exec -- python train.py`. It only waits; it does not start or repair sync.

Result collection via sync: add a second mapping with its own direction — no volume YAML needed (okdev auto-provisions the remote volume; the next `up` asks for `--reconcile`):

```yaml
sync:
  paths:
    - .:/workspace                 # code (bi or up)
    - local: ./collected           # may nest inside the repo
      remote: /data/results
      direction: down              # pod is the authority; local writes can't clobber results
```

Point the job's output at the mapping's remote root (`/data/results` above). This covers the hub pod; fleet-wide collection stays on `okdev jobs logs` / `okdev cp`.

## File Transfer

Use:

- `okdev cp ./local/path :/remote/path` for uploading files to a session pod
- `okdev cp :/remote/path ./local/path` for downloading files from a session pod
- `--all` or `--role` to fan out copies across multiple pods
- downloads from multiple pods are written into per-pod subdirectories
- single-file downloads resume automatically from a partial `<dest>` or `<dest>.okdev-part`; rerun `okdev cp` to continue, and add `--verify` to check SHA-256

## Teardown

Use:

- `okdev down`
- `okdev down --wait` when the user wants workload removal confirmed
- `okdev prune --ttl-hours <n>` for cleanup workflows
