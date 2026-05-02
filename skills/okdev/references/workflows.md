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

- `okdev ssh` for the managed interactive shell
- `ssh okdev-<session>` for plain SSH
- `okdev exec` for multi-pod parallel command execution (pdsh-style fanout with `--role`, `--label`, `--pod` targeting, `--detach` for background runs, and `--fanout` concurrency control)

If attachable pods are involved, explain that interactive targeting follows attachable-pod selection rules rather than arbitrary pod choice.

## Sync

Default guidance:

- `okdev up` already starts sync and waits for initial convergence
- `okdev status --details` shows whether sync is active
- `okdev sync --foreground` is useful when the user needs live troubleshooting
- `okdev sync --reset` is the standard repair path for stale local sync state

## File Transfer

Use:

- `okdev cp ./local/path :/remote/path` for uploading files to a session pod
- `okdev cp :/remote/path ./local/path` for downloading files from a session pod
- `--all` or `--role` to fan out copies across multiple pods
- downloads from multiple pods are written into per-pod subdirectories

## Teardown

Use:

- `okdev down`
- `okdev down --wait` when the user wants workload removal confirmed
- `okdev prune --ttl-hours <n>` for cleanup workflows
