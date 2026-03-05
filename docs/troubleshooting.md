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

## Port-forward disconnects

- Re-run `okdev ports`; transient network drops can interrupt tunnels.
- Confirm container process is listening on target remote port.
