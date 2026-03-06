# Troubleshooting

## Pod stuck Pending

- Run `okdev up`; if readiness fails, inspect embedded pod diagnostics in the error.
- Common causes: GPU resource unavailable, node selectors too strict, quota limits.

## Permission denied from Kubernetes

- Validate current kube context: `kubectl config current-context`
- Check namespace RBAC for Pod/PVC operations.

## Sync failures

- Ensure `tar` is available locally and inside container image.
- Verify mount path exists and is writable in the container.
- Start with one-way sync (`--mode up` or `--mode down`) to isolate direction issues.
- `okdev` auto-installs local Syncthing; ensure the syncthing sidecar is enabled in pod spec (`sync.engine=syncthing`).
- Current syncthing mode supports one `local:remote` mapping.
- If pod events show `ErrImagePull` with GHCR `403 Forbidden`, the image package is not publicly pullable. Set `spec.sync.syncthing.image` to a public image, or make the GHCR package public.

## Port-forward disconnects

- Re-run `okdev ports`; transient network drops can interrupt tunnels.
- Confirm container process is listening on target remote port.

## SSH connect issues

- Ensure an SSH server is running in the dev container.
- Use `okdev ssh --setup-key` once to install your public key in the pod.
- If port `2222` is busy locally, pass `okdev ssh --local-port <port>`.
