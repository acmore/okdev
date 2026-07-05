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

Direction: `spec.sync.paths[].direction: bi|up|down` sets the folder direction on the local↔hub edge for that mapping (`down` = pod is the authority, local writes can never clobber pod results). It applies to the mapping's whole folder — finer per-subpath direction is impossible in Syncthing. Mesh receiver pods are always receiveonly regardless.

Multiple mappings are supported when their roots are disjoint on both sides (each becomes its own folder with its own direction). Only the FIRST (primary) mapping fans out to mesh receivers; additional mappings are local↔hub only, so sync-based result collection covers the hub pod — fleet-wide results still go through `okdev jobs logs` / `okdev cp`.

## PyTorchJob

For PyTorchJob questions, keep these points in mind:

- manifests are usually generated beside `.okdev/okdev.yaml`
- role-specific pod templates are injected from manifest paths
- interactive targeting and sync behavior may differ between master and worker pods
- `okdev up` fails fast if any pod enters `Failed` before readiness; if that same `okdev up` created or recreated the PyTorchJob, okdev also deletes it and clears local session state (see `references/troubleshooting.md`, "Auto-Cleared Failed Jobs / PyTorchJobs"). Reused PyTorchJobs are left in place.

## Inter-Pod SSH

When a multi-pod session includes inter-pod SSH:

- `okdev up` configures session-scoped inter-pod SSH setup
- the participating session pods receive SSH host entries for the other session pods
- questions about inter-pod SSH should be answered in terms of `okdev` session behavior, not generic ad hoc SSH setup

Inter-pod SSH is toggled with `spec.ssh.interPod: true`. Consult `docs/command-reference.md` and `docs/config-manifest.md` for the current details.

If a user asks whether multi-pod inter-pod SSH should work, verify whether:

- `spec.ssh.interPod: true` is set and the session came up (or was reconciled) after setting it
- the workload has more than one participating pod
- the session came up successfully
- sidecar-related expectations for injected templates are satisfied by effective runtime behavior

## Gateway Fanout

With `spec.ssh.interPod: true`, multi-pod `okdev exec --json` and `okdev jobs list` route through an in-cluster gateway pod by default (`spec.exec.fanoutMode: auto`): okdev runs one apiserver exec against the gateway, and the gateway reaches every other pod over the pod network via interpod SSH. This is the reliable path for large fleets — it removes the per-pod apiserver exec streams that caused dropped/empty results at scale.

Key facts when helping users:

- `auto` uses the gateway when interpod SSH is on and ≥4 pods are targeted; `gateway` forces it; `direct` disables it (`spec.exec.fanoutMode`)
- the fanout driver ships in the sidecar image; the dev container does not need an ssh client
- per-pod envelope `status` (`responded`/`unreachable`/`timeout`/`error`/`missing`) tells "empty output" apart from "no response"; `--require-all` turns any non-responding pod into a non-zero okdev exit
- an older sidecar image (no fanout driver) makes okdev print a fallback notice on stderr and use direct per-pod exec; rebuild/publish the sidecar to enable the gateway path
- for `--detach` and streaming (non-`--json`) runs the direct per-pod path is still used
