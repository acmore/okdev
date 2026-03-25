# Troubleshooting

## Pod Stuck In `Pending`

- Run `okdev up` and inspect the readiness diagnostics emitted by the CLI.
- Typical causes: unschedulable GPU requests, restrictive node selectors/affinity, resource quota limits.

## Kubernetes Permission Errors

- Validate current kube context: `kubectl config current-context`
- Confirm namespace RBAC allows Pod/PVC create/update/delete and Pod exec/port-forward.

## Sync Failures

- Validate workspace path exists and is writable in the container.
- Isolate directionality with one-way runs: `okdev sync --mode up`, `okdev sync --mode down`.
- `sync.engine=syncthing` requires local Syncthing bootstrap (handled by `okdev`) and sidecar image pull success.
- Current Syncthing implementation supports a single `local:remote` mapping.
- If you see `ErrImagePull` / GHCR `403`, use a publicly pullable `spec.sidecar.image` or adjust registry permissions.

## Port Forward Disconnects

- Re-run `okdev ports` (or `okdev up`) to re-establish forwarding.
- Confirm target process is listening on the configured remote port inside the dev container/session.

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
- If tmux login fails with `missing or unsuitable terminal` (for example `xterm-ghostty`), upgrade to a sidecar image that includes terminal fallback handling, then recreate the pod with `okdev down && okdev up`.

## Local State Files

- okdev stores local runtime state in `~/.okdev/` (locks, logs, SSH metadata, sync state).
- If behavior looks stale after upgrades, inspect and clean targeted files under `~/.okdev/` instead of deleting project files.
