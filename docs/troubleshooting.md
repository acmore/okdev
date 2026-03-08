# Troubleshooting

## Pod Stuck In `Pending`

- Run `okdev up` and inspect the readiness diagnostics emitted by the CLI.
- Typical causes: unschedulable GPU requests, restrictive node selectors/affinity, resource quota limits.

## Kubernetes Permission Errors

- Validate current kube context: `kubectl config current-context`
- Confirm namespace RBAC allows Pod/PVC create/update/delete and Pod exec/port-forward.

## Sync Failures

- Validate workspace path exists and is writable in the container.
- Isolate directionality with one-way runs:
  - `okdev sync --mode up`
  - `okdev sync --mode down`
- `sync.engine=syncthing` requires:
  - local Syncthing bootstrap (handled by `okdev`)
  - sidecar image pull success
- Current Syncthing implementation supports a single `local:remote` mapping.
- If you see `ErrImagePull` / GHCR `403`, use a publicly pullable `spec.sidecar.image` or adjust registry permissions.

## Port Forward Disconnects

- Re-run `okdev ports` (or `okdev up`) to re-establish forwarding.
- Confirm target process is listening on the configured remote port inside the dev container/session.

## SSH Connection Errors

- Ensure `okdev-sidecar` is healthy and listening on `22`.
- Run `okdev ssh --setup-key` at least once per key pair.
- If local bind conflicts occur, use `okdev ssh --local-port <port>`.
- Verify managed entry exists in `~/.ssh/config`:
  - `Host okdev-<session>`
- For unstable links, increase:
  - `spec.ssh.keepAliveIntervalSeconds`
  - `spec.ssh.keepAliveTimeoutSeconds`
