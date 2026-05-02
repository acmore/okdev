# okdev Multi-Pod Notes

## When This Applies

Read this file for:

- `job`, `generic`, or `pytorchjob` workloads
- attachable pod questions
- multi-pod sync expectations
- inter-pod SSH questions

Use `docs/config-manifest.md` and `docs/command-reference.md` as the source of truth for exact field and command behavior.

## Manifest-Backed Workloads

`okdev` supports manifest-backed workloads with:

- `spec.workload.type: job`
- `spec.workload.type: generic`
- `spec.workload.type: pytorchjob`

These usually use `.okdev/okdev.yaml` plus companion manifests under `.okdev/`.

## Attachable Pods

Interactive access is shaped by attachable pods.

Useful guidance:

- not every pod is necessarily attachable
- attachable selection matters for `okdev ssh` and interactive targeting
- `okdev exec` is often the better tool when the user wants commands across multiple pods or specific roles

If the user wants to understand why one pod is chosen, consult the command/config docs before describing the selection behavior.

## Sync Expectations

Multi-pod sync behavior depends on workload layout:

- some setups rely on a shared PVC-backed workspace
- others rely on sidecars and Syncthing mesh behavior

Do not assume all pods independently sync from the local machine in the same way. Check the workload pattern first.

## PyTorchJob

For PyTorchJob questions, keep these points in mind:

- manifests are usually generated beside `.okdev/okdev.yaml`
- role-specific pod templates are injected from manifest paths
- interactive targeting and sync behavior may differ between master and worker pods

## Inter-Pod SSH

When `spec.ssh.interPod: true` is enabled:

- `okdev up` configures session-scoped inter-pod SSH setup
- the participating session pods receive SSH host entries for the session pods
- questions about inter-pod SSH should be answered in terms of `okdev` config and session behavior, not generic ad hoc SSH setup

If a user asks whether multi-pod inter-pod SSH should work, verify whether:

- the workload has more than one participating pod
- the session came up successfully
- sidecar-related expectations for injected templates are satisfied by effective runtime behavior
