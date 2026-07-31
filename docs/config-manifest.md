# Config Manifest

okdev is configured by a single YAML manifest at `.okdev/okdev.yaml`, with any workload manifests beside it. Configs written before okdev always used the folder — a flat `.okdev.yaml`, or a bare `okdev.yaml` — keep working and are not migrated.

## Config Resolution

Priority: `-c/--config` flag > `OKDEV_CONFIG` environment variable > parent-directory discovery. Discovery walks up from the current directory checking `.okdev/okdev.yaml`, `.okdev.yaml`, then `okdev.yaml` per directory, stopping at the outermost enclosing git root — a submodule's file-shaped `.git` does not cut the walk short, so a config at the superproject root is found from inside a submodule.

Discovery can only find configs on the current directory's ancestor chain. When working from a directory **outside** the repo (a scratch/experiment dir), use one of the from-anywhere addressing paths instead:

- `--session <name>` resolves a saved session's config from session metadata — per-invocation and explicit, the right tool when juggling several sessions;
- `OKDEV_CONFIG=/path/to/.okdev.yaml` — set once for a single-project loop. If the env var steers okdev away from a config that discovery *would* have found from the current directory, a warning names both paths (an exported env var silently overriding the local repo is the classic footgun); in a scratch dir it stays silent.

Unrecognized `spec` fields (typos like `spec.context` for `spec.kubeContext`, or fields removed from the schema) do not fail parsing — `okdev up` warns about them, listing the known keys, so a typo cannot silently cost a debugging session.

## Skeleton

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec: {}
```

- `apiVersion`: must be `okdev.io/v1alpha1`
- `kind`: must be `DevEnvironment`
- `metadata.name` (`string`, required): logical project/environment name.

## `spec` Fields

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `namespace` | `string` | `default` | Kubernetes namespace |
| `kubeContext` | `string` | — | Kubeconfig context for okdev commands |
| `template` | `object` | — | Template reference and resolved custom variables from `okdev init --template` |
| `session` | `object` | — | Session naming and lifecycle policy |
| `agents` | `array` | — | Coding agent configuration and local auth conventions |
| `volumes` | `array` | — | Kubernetes volume definitions |
| `sync` | `object` | — | File sync configuration |
| `ports` | `array` | — | Port forwarding rules |
| `ssh` | `object` | — | SSH and tmux settings |
| `lifecycle` | `object` | — | Post-create, post-sync, and pre-stop hooks |
| `workload` | `object` | — | The workload to run. The *effective* one: after profile selection this holds whichever profile is active |
| `workloads` | `array` | — | Named workload profiles this session can switch between |
| `defaultWorkload` | `string` | first entry | Which profile a **new** session starts on |
| `sidecar` | `object` | — | Sidecar container image |

---

## `spec.template`

Records the template reference and resolved custom variables used to generate a config. This block is written for non-built-in templates and for templates that declare custom variables.

```yaml
spec:
  template:
    name: pytorch-ddp
    vars:
      numWorkers: 4
      baseImage: pytorch/pytorch:latest
```

`okdev migrate --template <name>` uses `spec.template.vars` as defaults, applies any `--set key=value` overrides, re-renders the template, preserves existing config values over template defaults, and regenerates any companion files declared in template frontmatter `files`.

---

## `spec.agents`

Configures supported coding-agent CLIs for the session container.

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `name` | `string` | — | Supported agent name: `claude-code`, `codex`, `gemini`, or `opencode` |
| `auth.env` | `string` | convention | Local env var to check during setup-time auth preparation |
| `auth.localPath` | `string` | convention | Local auth file path to stage into the session container |

**Validation:** agent names must be unique; `auth.env` must be a valid env var name; `auth.localPath` must resolve on the local machine.

Convention defaults:

| Agent | Default `auth.env` | Default `auth.localPath` |
|-------|--------------------|--------------------------|
| `claude-code` | — | — |
| `codex` | — | `~/.codex/auth.json` |
| `gemini` | — | — |
| `opencode` | — | — |

```yaml
spec:
  agents:
    - name: claude-code
    - name: codex
    - name: gemini
    - name: opencode
```

Override the Codex auth file when your environment uses a different local path:

```yaml
spec:
  agents:
    - name: codex
      auth:
        localPath: ~/.codex/company-auth.json
```

Phase 1 currently supports:

- `okdev up` install checks for configured agent CLIs
- `okdev up` best-effort installation of a modern Node/npm runtime via `nvm` first when a configured agent CLI needs it and the dev image has `bash` and `curl`
- `okdev up` setup-time staging of local auth files into reserved runtime paths for dedicated sessions
- `okdev down` best-effort cleanup of staged agent auth before session deletion
- `okdev agent list` to show configured agents, install status, and whether auth is staged in the current session container

Current limits:

- env-only auth sources are detected but still require manual login inside the container
- existing real auth files inside the container are left alone instead of being overwritten

Users continue to launch agent CLIs manually through `okdev ssh`, plain SSH, or editor remote sessions.

---

## `spec.session`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `defaultNameTemplate` | `string` | — | Go template for inferred session name |

```yaml
spec:
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .User }}"
```

Note: earlier versions accepted `ttlHours` / `idleTimeoutMinutes` here and shipped an `okdev prune` command around them. Nothing cluster-side ever enforced those values, so the fields and the command were removed; leftover fields in existing configs are ignored. Use your platform's own reclamation (or a cron running `okdev down`) for cleanup policies.

---

## Volumes (`spec.volumes` is removed)

**Volumes belong to the workload manifest**, declared beside the containers that
mount them, exactly as in any Kubernetes object.

`spec.volumes` used to hold the sources while the manifest held the mounts. That
split one object across two files, and — because a name declared in both went to
the manifest — a workspace PVC in the config silently became whatever the
manifest said. Run `okdev migrate` to move them; it copies them into every
workload's manifest and leaves any name the manifest already declares alone,
naming it.

### What okdev still injects

okdev does not touch what the manifest declares. Its one exception is the sync
target:

- **`workspace`** — injected as `emptyDir: {}` **only when the manifest declares
  no volume by that name.** Declare it yourself — a PVC, an `ephemeral`
  volumeClaimTemplate, an `emptyDir` with a `sizeLimit` — and okdev uses exactly
  that.
- **`syncthing-home`, `okdev-runtime`** — the sidecar's own state, always
  injected. They belong to okdev's machinery, not to your object.

okdev mounts `workspace` into the sidecar as well as the attach container, and
the mount path follows the `volumeMounts` entry for `workspace` in the manifest
(default `/workspace`).

### Persistent workspace

```yaml
# .okdev/pod.yaml
spec:
  containers:
    - name: dev
      volumeMounts:
        - name: workspace
          mountPath: /workspace
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: team-workspace
```

### Per-session storage

`ephemeral` gives each session its own claim, created and deleted with the pod —
no PVC to manage by hand:

```yaml
  volumes:
    - name: workspace
      ephemeral:
        volumeClaimTemplate:
          spec:
            accessModes: ["ReadWriteOnce"]
            resources:
              requests:
                storage: 50Gi
```

### PVC

```yaml
spec:
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: team-workspace
    - name: datasets
      persistentVolumeClaim:
        claimName: shared-datasets
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      volumeMounts:
        - name: workspace
          mountPath: /workspace
        - name: datasets
          mountPath: /data
          readOnly: true
```

### emptyDir (Scratch Space)

```yaml
spec:
  volumes:
    - name: cache
      emptyDir: {}
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      volumeMounts:
        - name: cache
          mountPath: /tmp/build-cache
```

### configMap and secret

```yaml
spec:
  volumes:
    - name: app-config
      configMap:
        name: my-app-config
    - name: cloud-creds
      secret:
        secretName: cloud-credentials
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      volumeMounts:
        - name: app-config
          mountPath: /etc/my-app
          readOnly: true
        - name: cloud-creds
          mountPath: /var/run/secrets/cloud
          readOnly: true
```

### Ephemeral (Per-Session PVC)

Created with the pod, removed when it's deleted.

```yaml
spec:
  volumes:
    - name: models
      ephemeral:
        volumeClaimTemplate:
          spec:
            accessModes: ["ReadWriteOnce"]
            resources:
              requests:
                storage: 50Gi
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      volumeMounts:
        - name: models
          mountPath: /models
```

---

## `spec.sync`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `engine` | `string` | — | Sync engine (currently only `syncthing`) |
| `paths` | `[]entry` | — | Sync mappings. Each entry is either the compact string `local:remote`, or a mapping `{local, remote, direction}` with an optional per-path `direction`. Multiple mappings are allowed as long as their roots are disjoint on both sides (equal or nested roots are rejected) |
| `paths[].direction` | `string` | `bi` | Sync direction for this mapping's folder: `bi` (bidirectional), `up` (local is the authority: local sendonly, pod receiveonly), `down` (pod is the authority: pod sendonly, local receiveonly) |
| `syncthing.version` | `string` | `v1.29.7` | Local Syncthing binary version |
| `syncthing.autoInstall` | `bool` | `true` | Auto-install local Syncthing |
| `syncthing.image` | `string` | `ghcr.io/acmore/okdev:<version>` | Sidecar image (fallback: `edge`) |
| `syncthing.rescanIntervalSeconds` | `int` | `300` | Fallback full rescan interval; `0` disables periodic rescans |
| `syncthing.relaysEnabled` | `bool` | `false` | Allow Syncthing relay fallback when direct connectivity is unavailable |
| `syncthing.compression` | `bool` | `false` | Use Syncthing `always` compression for peer connections instead of the default `metadata` mode |
| `syncthing.versioningDays` | `int` | `30` | Keep files overwritten or deleted by sync in `.stversions` for this many days (staggered versioning, both sides); `0` disables |

**Validation:** `engine` must be `syncthing`; each `paths[]` entry must have non-empty local and remote; `direction` must be `bi`, `up`, or `down`; mapping roots must be pairwise disjoint on both the local and the remote side (lexical check); `versioningDays` must be `>= 0`.

**Multiple mappings.** Each mapping becomes its own syncthing folder with its own direction. The first entry is the **primary** mapping: it keeps the legacy folder identity (existing sessions upgrade in place), it is the folder shared to mesh receiver pods, and it is the only remote cleared by `okdev up --reset-workspace` / `okdev sync reset-remote` — additional mappings (typically datasets or results) are never touched by resets and stay local↔hub only. Removing a mapping from the config removes its folder from both syncthing instances on the next `okdev up`. A typical split:

```yaml
paths:
  - .:/workspace                     # primary: code, bidirectional
  - local: ./collected               # nested inside the repo — allowed:
    remote: /data/results            # okdev excludes it from the primary folder
    direction: down                  # pod-generated results, pod is authority
  - local: ../datasets               # disjoint roots work too
    remote: /data/datasets
    direction: up                    # dataset push, local is authority
```

**Nested local roots.** An additional mapping's local root may live *inside* the primary mapping's local root (`./collected` under `.`). okdev maintains a managed block in the primary root's `.stignore` (`// okdev:begin/end managed sync excludes`) so the subtree travels only through its own folder — the block is written before the sync daemon starts, so the exclusion is in force from the first scan. Any other local overlap (equal roots, nesting between additional mappings, an additional root containing the primary) is rejected, and **remote roots must always be disjoint**.

**Removing a mapping never widens the primary folder.** When a nested mapping is removed, its folder is pruned from both syncthing instances and local files are kept, but the `.stignore` entry is *retained* as a tombstone (with an explanatory comment) — otherwise the subtree would suddenly bulk-sync through the code channel. To fold the directory back into the primary folder, delete its line from `.stignore`. `status --details` lists both active and retained excludes.

**Volumes are provisioned automatically.** An additional mapping's remote root must be visible to the okdev sidecar (its syncthing serves the remote side), which means it must live on a volume — a root on the container's own filesystem cannot sync. You don't need to write any volume YAML: okdev auto-provisions an emptyDir at any extra remote root not already covered by a target-container volume mount, and mirrors the target container's mounts into the sidecar. Adding or removing a mapping changes the pod spec, so the next `okdev up` asks for `--reconcile` (pod recreate). Auto-provisioned emptyDirs don't survive pod recreation — fine for both directions (`up` re-pushes from local, `down` results are mirrored locally); mount your own PVC at the root if the data must persist, and okdev will use it instead. Sync startup still probes each extra root and fails with a clear error if it is not shared with the sidecar.

**Sync direction.** `direction` applies to a mapping's whole synced folder — finer per-subpath direction inside one folder is impossible with Syncthing (ignore rules live in the directory itself; per-path folder types were rejected upstream). Pick the direction by what the folder carries:

- `bi` (default): source code you edit on both sides. Either side can overwrite the other, so **keep generated outputs (results, checkpoints, logs) out of the synced path** — write them to a non-synced location and fetch with `okdev cp` or `okdev jobs logs`. Versioning (`.stversions/`) is the safety net, not the design.
- `up`: the folder is a code drop. Nothing the pod writes can ever touch local files; pod-side writes into the folder are flagged by Syncthing but not propagated.
- `down`: the folder carries pod-generated results. A stray local write (for example an empty file from a failed redirect) can never clobber the pod's data. `okdev up --reset-workspace` and `okdev sync reset-remote` are rejected in this direction because they reseed the remote from local.

An explicit `okdev sync --mode` overrides the configured direction for that invocation. Changing `direction` takes effect on the next `okdev up` (it restarts sync with the new folder types). Mesh receiver pods are always receiveonly regardless of direction.

```yaml
# compact form (direction defaults to bi):
paths:
  - .:/workspace

# structured form with an explicit direction:
paths:
  - local: .
    remote: /workspace
    direction: up
```

Local ignore rules come from the synced workspace's `.stignore`. `okdev init` writes a starter `.stignore` for built-in templates, and `okdev up` creates one with default patterns if the local sync root does not already have one. Editing `.stignore` takes effect automatically as Syncthing notices the change, but it does not remove files that were already synced to the remote workspace. For faster initial syncs, consider ignoring large generated build outputs or local test artifacts such as `debug/`, `release/`, caches, and dataset directories when they do not need to exist remotely.

The `syncthing.version` field controls the local binary on your machine. The Syncthing binary inside the sidecar comes from `spec.sidecar.image`.

**File versioning (data-loss safety net).** The managed folder syncs bidirectionally in steady state, so a bad write on one side (for example an empty file created locally by a failed command redirect) propagates and overwrites the real file on the other side. With versioning enabled (the default), the side applying an incoming overwrite or deletion first archives the previous version into `.stversions/` at the sync root — on the pod under the remote workspace, locally under the local sync root — and staggered cleanup drops versions older than `versioningDays`. `.stversions` is Syncthing-internal and is never synced back. To recover a clobbered file, copy it out of `.stversions/` on the side that had the good copy (versioned names carry a `~YYYYMMDD-HHMMSS` suffix).

```yaml
spec:
  sync:
    engine: syncthing
    syncthing:
      version: v1.29.7
      autoInstall: true
      rescanIntervalSeconds: 300
      relaysEnabled: false
      compression: false
      versioningDays: 30
    paths:
      - .:/workspace
```

---

## `spec.ports`

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `name` | `string` | no | Label for the port rule |
| `local` | `int` | yes | Local listening port |
| `remote` | `int` | yes | Remote target port |
| `direction` | `string` | `forward` | `forward` for local-to-remote, `reverse` for remote-to-local |

**Validation:** ports must be `1..65535`; forward mappings must have unique `local` ports; reverse mappings must have unique `remote` ports.

```yaml
spec:
  ports:
    - name: app
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
    - name: api-reverse
      local: 3000
      remote: 3000
      direction: reverse
```

---

## `spec.ssh`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `user` | `string` | `root` | SSH user |
| `privateKeyPath` | `string` | — | Path to SSH private key |
| `autoDetectPorts` | `bool` | `true` | Auto-detect listening ports in the container |
| `forwardAgent` | `bool` | `false` | Forward the local SSH agent for live `okdev ssh` sessions |
| `interPod` | `bool` | `false` | Enable passwordless SSH between session pods using a session-scoped key |
| `persistentSession` | `bool` | `true` | Enable tmux-backed interactive sessions |
| `keepAliveIntervalSeconds` | `int` | `10` | SSH keepalive interval |
| `keepAliveTimeoutSeconds` | `int` | `10` | SSH keepalive timeout |

**Validation:** both keepalive values must be `> 0`; timeout must be `>= interval`.

When `forwardAgent: true`, `okdev ssh` forwards the local `SSH_AUTH_SOCK` only for that live SSH session. This lets Git/SSH in the workload use your local agent for operations like `git push`, but it requires that your local machine already has an active ssh-agent with identities loaded via `ssh-add`.

When `interPod: true`, `okdev up` generates a session-scoped SSH key, installs it on all session pods, starts `okdev-sshd` on each pod, and writes per-pod SSH host entries inside the workload so you can `ssh <pod-name>` from one session pod to another. For every injected pod template that participates in the session, okdev treats sidecars as effectively enabled at runtime even if the manifest still says `sidecar: false`.

```yaml
spec:
  ssh:
    user: root
    privateKeyPath: ~/.okdev/ssh/id_ed25519
    autoDetectPorts: true
    forwardAgent: false
    interPod: false
    persistentSession: true
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
```

---

## `spec.exec`

| Field | Type | Default | Description |
|-------|------|---------|-------------|
| `fanoutMode` | `string` | `auto` | How multi-pod exec fanout reaches pods: `auto`, `gateway`, or `direct` |

- `auto` — use gateway fanout when interpod SSH is available (`spec.ssh.interPod: true`), every targeted pod has a pod IP, and at least 4 pods are targeted (or `--gateway` names one explicitly); otherwise direct per-pod exec.
- `gateway` — always attempt gateway fanout for `exec --json` and `jobs list`, regardless of pod count. Falls back to direct exec when the gateway driver is unavailable (e.g. an older sidecar image).
- `direct` — always exec into each pod through the apiserver (pre-gateway behavior).

Gateway fanout routes a multi-pod `exec --json` / `jobs list` through a single session pod (the workload master when present): okdev runs the fanout driver there with one apiserver exec, and the driver reaches every other pod over the pod network via the interpod SSH channel. At high pod counts this replaces N proxied apiserver exec streams with 2, which removes the apiserver as the bottleneck that caused dropped streams and empty results. The driver ships in the sidecar image; the dev container needs no ssh client.

```yaml
spec:
  exec:
    fanoutMode: auto
```

---

## `spec.lifecycle`

| Field | Type | Description |
|-------|------|-------------|
| `postCreate` | `string` | Command run once on the target pod after environment creation |
| `postSync` | `string` | Command run once on **all** pods after initial shared-workspace sync completes |
| `preStop` | `string` | Command run before pod termination |

`postSync` is useful for multi-pod workloads where every pod needs code-dependent
setup (e.g. editable Python installs). It assumes the synced workspace is
shared across pods, typically via a common PVC mounted at the workspace path.
`okdev up` blocks until the initial syncthing sync finishes for that shared
workspace, only treating it as complete once both local and remote pending-byte
counters reach zero, then executes the command on every running session pod in parallel.
Each pod is tracked via the `okdev.io/post-sync-done` annotation to prevent
re-execution. Falls back to `.okdev/post-sync.sh` if no explicit command is
configured.

```yaml
spec:
  lifecycle:
    postCreate: uv sync --dev
    postSync: pip install -e .
    preStop: pkill -f my-dev-server || true
```

---

## `spec.sidecar`

| Field | Type | Description |
|-------|------|-------------|
| `image` | `string` | Sidecar container image (use release-aligned tag) |
| `resources` | `object` | Kubernetes container resources for the injected `okdev-sidecar` container |

```yaml
spec:
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
    resources:
      requests:
        cpu: "250m"
        memory: "512Mi"
      limits:
        cpu: "250m"
        memory: "512Mi"
```

This applies to the injected `okdev-sidecar` container. To get `Guaranteed` QoS, set equal requests and limits on every container in the pod, including both `dev` and `okdev-sidecar`.

---

## `spec.workloads` and `spec.defaultWorkload`

Declare several workloads in one config and switch the session between them with
`okdev workload use <name>` — no re-`init`, and no second session.

```yaml
spec:
  workloads:
    - name: dev
      type: pod
      manifestPath: pod.yaml
    - name: train
      type: pytorchjob
      manifestPath: pytorchjob.yaml
      inject:
        - path: spec.pytorchReplicaSpecs.Worker.template
  defaultWorkload: dev
```

A profile owns exactly `name`, `type`, `manifestPath`, `inject`, and `attach`.
Everything else — `ports`, `sync`, `sidecar`, `volumes`, `namespace` — is shared
and identical across profiles. To vary those, use a separate session.

### The singular `spec.workload`

`spec.workload` still works, unchanged, and is desugared into a one-entry list:

| what your config says | what okdev sees |
|---|---|
| `spec.workload: {type: job, …}` | one profile named `default` carrying those fields |
| neither `spec.workload` nor `spec.workloads` | one profile named `default` of type `pod` |
| `spec.workloads: [...]` | exactly those profiles |

After selection, `spec.workload` holds **the effective workload** — whichever
profile is active for this command. Reading it always gives you the workload
actually in play.

Every profile needs its own `manifestPath`, `pod` included — `okdev init
--workload pod --workload-name <name>` scaffolds one for you.

Workload manifests always live in `.okdev/`, whichever shape the config has.

### Which profile is active

```
okdev up --workload <name>  →  session pin  →  spec.defaultWorkload  →  workloads[0]
```

The pin lives with the session, not the repo: two sessions in one repo would
otherwise contend for one slot, and a repo-level pin moved by `git pull` could
silently swap out a running workload. `spec.defaultWorkload` therefore only
decides where a **new** session starts — it never moves an existing one.

Switching is always explicit and always destructive: `okdev up` deletes the
running workload before creating the newly selected one, and prompts first
unless `--yes` is given. See `okdev workload` in the command reference for the
full contrast against `okdev use <session>`.

---

## The pod manifest (`spec.podTemplate` is removed)

Pod workloads used to be defined inline under `spec.podTemplate`. They are
manifests now, like every other workload type, which makes them lintable by
normal Kubernetes tooling and removes pod's special cases. The sidecar is
injected at the object root, because a Pod minus `apiVersion`/`kind` *is* a pod
template.

Run `okdev migrate` to convert an existing config: it writes `.okdev/pod.yaml`
and points the workload at it. The first
`okdev up` afterwards reports drift and offers to recreate — the Pod it builds
is unchanged, only the config's description of it moved.

A config that never declared a `podTemplate` ran on a container okdev injected
by default; `okdev migrate` writes that out as a real manifest too, so there is
always something to edit rather than a hidden default.

Use the manifest for the dev container image, resources, env vars, scheduling,
labels, and extra sidecars.

```yaml
spec:
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: nvidia/cuda:12.4.1-devel-ubuntu22.04
      command: ["sleep", "infinity"]
      env:
        - name: HF_HOME
          value: /workspace/.cache/huggingface
      resources:
        requests:
          cpu: "8"
          memory: 32Gi
          nvidia.com/gpu: "1"
        limits:
          cpu: "16"
          memory: 64Gi
          nvidia.com/gpu: "1"
```

### Interactive Container Selection

`okdev` needs one container to treat as the main interactive container for `okdev ssh`, `okdev exec`, runtime mounts, and helper env vars.

- For `spec.workload.type: pod`, the default interactive container name is `dev`.
- For manifest-backed workloads (`job`, `generic`, `pytorchjob`), the default interactive container name is also `dev`.
- If your main container uses a different name, set `spec.workload.attach.container` to that container name.
- For manifest-backed workloads, if no configured attach container is found in an injected pod template, okdev falls back to the first container in that pod template.

This means env vars like `LANG` should usually be added to the container that okdev will attach to.

Manifest-backed workload files are rendered as okdev runtime templates before apply. Use `{{ .WorkloadName }}` anywhere the manifest should follow the generated per-run Kubernetes workload name, and use `{{ .SessionName }}` or `{{ .RunID }}` for session/run labels. `{{ .ConfigName }}` is the stable `metadata.name` from the okdev config, and `{{ .WorkloadType }}` is `job`, `generic`, or `pytorchjob`. Static fields remain static, so the manifest author controls which labels, selectors, and names change per run.

Default pod example:

```yaml
spec:
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: ubuntu:22.04
      command: ["sleep", "infinity"]
      env:
        - name: LANG
          value: en_US.UTF-8
        - name: LC_ALL
          value: en_US.UTF-8
```

Non-default container example:

```yaml
spec:
  workload:
    attach:
      container: trainer
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: trainer
      image: python:3.12
      command: ["sleep", "infinity"]
      env:
        - name: LANG
          value: en_US.UTF-8
```

---

## Full Examples

### Minimal Web App

The simplest useful manifest. Ephemeral workspace, one synced directory, one forwarded port.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-app
spec:
  sync:
    engine: syncthing
    paths:
      - .:/workspace
  ports:
    - name: app
      local: 3000
      remote: 3000
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: node:22-bookworm
      command: ["sleep", "infinity"]
```

### Persistent Workspace (PVC)

Workspace survives pod restarts — no need to re-run `npm install` or `pip install` on every `okdev up`.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: backend-api
spec:
  namespace: dev
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: backend-workspace
  sync:
    engine: syncthing
    paths:
      - .:/workspace
    exclude:
      - node_modules/
      - dist/
  ports:
    - name: api
      local: 8080
      remote: 8080
    - name: debug
      local: 9229
      remote: 9229
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: node:22-bookworm
      command: ["sleep", "infinity"]
      resources:
        requests:
          cpu: "2"
          memory: 4Gi
```

### Python ML with GPU

Single GPU, persistent workspace, read-only dataset volume, auto-install dependencies on create.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: ml-training
spec:
  namespace: ai-dev
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: ml-workspace
    - name: datasets
      persistentVolumeClaim:
        claimName: shared-datasets
  sync:
    engine: syncthing
    paths:
      - .:/workspace
  ports:
    - name: jupyter
      local: 8888
      remote: 8888
    - name: tensorboard
      local: 6006
      remote: 6006
  ssh:
    persistentSession: true
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
  lifecycle:
    postCreate: |
      cd /workspace && pip install -e ".[dev]" 2>/dev/null || true
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: nvidia/cuda:12.4.1-devel-ubuntu22.04
      command: ["sleep", "infinity"]
      volumeMounts:
        - name: workspace
          mountPath: /workspace
        - name: datasets
          mountPath: /data
          readOnly: true
      resources:
        requests:
          cpu: "8"
          memory: 32Gi
          nvidia.com/gpu: "1"
        limits:
          cpu: "16"
          memory: 64Gi
          nvidia.com/gpu: "1"
```

### Multi-GPU LLM Fine-Tuning

4x GPU, model cache volume, long keepalive for stable training connections.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-finetune
spec:
  namespace: ai-team
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .User }}"
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: llm-workspace
    - name: models
      persistentVolumeClaim:
        claimName: model-cache
  sync:
    engine: syncthing
    paths:
      - .:/workspace
  ports:
    - name: vllm
      local: 8000
      remote: 8000
    - name: tensorboard
      local: 6006
      remote: 6006
    - name: wandb
      local: 8081
      remote: 8081
  ssh:
    persistentSession: true
    keepAliveIntervalSeconds: 60
    keepAliveTimeoutSeconds: 180
  lifecycle:
    postCreate: |
      cd /workspace && pip install -e ".[train]" 2>/dev/null || true
    preStop: |
      pkill -f vllm || true
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
  labels:
    team: ml-infra
spec:
  containers:
    - name: dev
      image: nvidia/cuda:12.4.1-devel-ubuntu22.04
      command: ["sleep", "infinity"]
      env:
        - name: HF_HOME
          value: /models
        - name: WANDB_DIR
          value: /workspace/wandb
      volumeMounts:
        - name: workspace
          mountPath: /workspace
        - name: models
          mountPath: /models
      resources:
        requests:
          cpu: "16"
          memory: 128Gi
          nvidia.com/gpu: "4"
        limits:
          cpu: "32"
          memory: 256Gi
          nvidia.com/gpu: "4"
  tolerations:
    - key: nvidia.com/gpu
      operator: Exists
      effect: NoSchedule
```

### Go Microservice (No Tmux)

Lightweight setup with tmux disabled for a plain SSH shell.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: auth-service
spec:
  sync:
    engine: syncthing
    paths:
      - .:/workspace
    exclude:
      - vendor/
  ports:
    - name: grpc
      local: 9090
      remote: 9090
    - name: metrics
      local: 2112
      remote: 2112
  ssh:
    persistentSession: false
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: golang:1.24-bookworm
      command: ["sleep", "infinity"]
      resources:
        requests:
          cpu: "4"
          memory: 8Gi
```

### Custom Kube Context and Namespace

Explicit cluster targeting for multi-cluster setups with node pool scheduling.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: staging-debug
spec:
  namespace: staging
  kubeContext: gke_myproject_us-central1_staging
  sync:
    engine: syncthing
    paths:
      - .:/workspace
  ports:
    - name: app
      local: 8080
      remote: 8080
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
  workload:
    type: pod
    manifestPath: pod.yaml
```

```yaml
# .okdev/pod.yaml
apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: python:3.12-slim
      command: ["sleep", "infinity"]
  nodeSelector:
    cloud.google.com/gke-nodepool: dev-pool
```
