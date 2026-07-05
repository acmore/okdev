# Troubleshooting

## Pod Stuck In `Pending`

- Run `okdev up` and inspect the readiness diagnostics emitted by the CLI.
- Typical causes: unschedulable GPU requests, restrictive node selectors/affinity, resource quota limits.
- For deeper cluster-side detail, run `kubectl -n <namespace> describe pod okdev-<session>` and inspect recent events.

## Recreate vs Reuse

- `okdev up` now reuses an existing session workload by default and reruns setup against it.
- If you changed the base image, pod spec, sidecar image, or other workload-shaping config and need a fresh pod, run:
  - `okdev down`
  - `okdev up`
- Use `okdev sync --reset` when only local/background sync state is stale and the workload itself is still correct.

## Kubernetes Permission Errors

- Validate current kube context: `kubectl config current-context`
- Confirm namespace RBAC allows Pod/PVC create/update/delete and Pod exec/port-forward.

## Sync Failures

- Validate workspace path exists and is writable in the container.
- Isolate directionality with one-way runs: `okdev sync --mode up`, `okdev sync --mode down` (an explicit `--mode` forces every mapping for that invocation).
- `sync.engine=syncthing` requires local Syncthing bootstrap (handled by `okdev`) and sidecar image pull success.
- Multiple mappings are supported (each is its own syncthing folder with its own `direction`); remote roots must be disjoint, and a local root nested inside the primary root is excluded from the primary folder via the managed block in its `.stignore`.
- If you see `ErrImagePull` / GHCR `403`, use a publicly pullable `spec.sidecar.image` or adjust registry permissions.
- If `okdev status --details` shows stale local sync state or a stale PID file, run `okdev sync --reset`. For mesh sessions, `--reset` also probes receiver health and repairs any broken mesh connections.
- `okdev status --details` shows live mesh health when receivers exist, including per-receiver connection and sync status.
- If the session pod was recreated, rerun `okdev up` to re-bootstrap local sync against the new pod.
- Re-run `okdev ports` if the sync connection depends on managed SSH forwarding that may have been interrupted.

Before treating "changes are not syncing" as a fault, rule out two by-design behaviors:

- **Direction contract**: with `direction: up` pod-side writes never reach local; with `down` local writes never reach the pod (syncthing marks them as local additions). `okdev status --details` shows each mapping's direction arrow.
- **Managed excludes**: a directory that belongs to (or belonged to) its own nested mapping is excluded from the primary folder. `status --details` lists active excludes and tombstones retained after a mapping was removed; delete the entry from the primary root's `.stignore` to fold the directory back into the primary folder.

## Recovering A File Overwritten By Sync

With the default `direction: bi` a folder is bidirectional in steady state, so a bad write on one side (e.g. an empty file from a failed `okdev exec ... > result.txt` redirect) can propagate and overwrite the real file on the other side. (Directional mappings — `up`/`down` — are structurally immune: the non-authoritative side's writes are never propagated.) Versioning (on by default, `spec.sync.syncthing.versioningDays`) archives the previous version on the side that applied the incoming change:

- On the pod: `<remote workspace>/.stversions/` (e.g. `/workspace/.stversions/`)
- Locally: `<local sync root>/.stversions/`

Versioned files carry a `~YYYYMMDD-HHMMSS` suffix before the extension (e.g. `results~20260703-141530.txt`). Copy the file back into place on the side that had the good copy and let sync propagate it. Versions older than `versioningDays` (default 30) are cleaned up automatically. Note versioning only protects against changes applied *by sync* — it does not version files you overwrite directly on the same machine.

## Coding Agent Setup Issues

- Check configured agents and staged auth with:
  - `okdev agent list`
  - `okdev status --details`
- `codex`, `gemini`, and `opencode` use npm-based installation. If install is skipped, check:
  - the dev container can run as root when package bootstrap is needed
  - `bash` and `curl` are present or installable
  - `node -v` and `npm -v` work inside the container
- `okdev` does not launch the agent for you; after setup, connect with `okdev ssh` and run the agent CLI manually.

## Port Forward Disconnects

- Re-run `okdev ports` (or `okdev up`) to re-establish forwarding.
- Confirm target process is listening on the configured remote port inside the dev container/session.
- Use `okdev ports --dry-run` to preview the managed forward set without rewriting SSH config.

## SSH Connection Errors

- Ensure `okdev-sshd` is running in the dev container on port `2222`.
- Run `okdev ssh --setup-key` at least once per key pair.
- If local bind conflicts occur, use `okdev ssh --local-port <port>`.
- Verify the managed entry exists in `~/.ssh/config`: `Host okdev-<session>`.
- For tmux-enabled sessions, use interactive TTY mode with `okdev ssh`.
- `ssh okdev-<session>` opens a plain shell by default.
- To bypass tmux for one `okdev ssh` connection, use `okdev ssh --no-tmux`.
- For unstable links, increase `spec.ssh.keepAliveIntervalSeconds` and `spec.ssh.keepAliveTimeoutSeconds`.
- If `ssh okdev-<session>` exits after a long stall, inspect `~/.okdev/logs/okdev.log` for proxy health events such as port-forward degradation or idle watchdog disconnects.
- Managed proxy sessions are designed to fail closed and return control to the local terminal rather than hang indefinitely on a dead port-forward.
- If you use Ghostty and terminal setup behaves incorrectly in `okdev ssh` or `ssh okdev-<session>`, upgrade both the local `okdev` binary and the sidecar image, then recreate the pod with `okdev down && okdev up`.
- A warning that SSH service is "not ready yet" means `okdev` is falling back to tunnel setup anyway; if the subsequent connection still fails, rerun `okdev up` and then retry `okdev ssh`.

## Local State Files

- okdev stores local runtime state in `~/.okdev/` (locks, logs, SSH metadata, sync state).
- If behavior looks stale after upgrades, inspect and clean targeted files under `~/.okdev/` instead of deleting project files.
