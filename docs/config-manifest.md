# Config Manifest

okdev is configured by a single YAML manifest. Simple pod setups default to `.okdev.yaml`; manifest-backed workload setups initialized by `okdev init` default to `.okdev/okdev.yaml` with generated workload manifests beside it.

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
| `sidecar` | `object` | — | Sidecar container image |
| `podTemplate` | `object` | — | Full Kubernetes PodSpec overlay |

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
| `ttlHours` | `int` | from template | Reserved for lifecycle policy |
| `idleTimeoutMinutes` | `int` | from template | Reserved for lifecycle policy |

**Validation:** `ttlHours >= 0`, `idleTimeoutMinutes >= 0`.

```yaml
spec:
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
```

---

## `spec.volumes`

Uses native Kubernetes `corev1.Volume` schema. Define volume sources here and mount them via `spec.podTemplate.spec.containers[*].volumeMounts`.

### Workspace Behavior

- If no volume named `workspace` is provided, okdev injects `emptyDir: {}` automatically.
- okdev ensures `workspace` is mounted on both the `dev` container and sidecar.
- Mount path defaults to `/workspace`, or follows the `volumeMounts` entry for `workspace` in `podTemplate`.

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
  podTemplate:
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
  podTemplate:
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
  podTemplate:
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
  podTemplate:
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
| `paths` | `[]string` | — | Mappings in `local:remote` format (max 1 entry) |
| `syncthing.version` | `string` | `v1.29.7` | Local Syncthing binary version |
| `syncthing.autoInstall` | `bool` | `true` | Auto-install local Syncthing |
| `syncthing.image` | `string` | `ghcr.io/acmore/okdev:<version>` | Sidecar image (fallback: `edge`) |
| `syncthing.rescanIntervalSeconds` | `int` | `300` | Fallback full rescan interval; `0` disables periodic rescans |
| `syncthing.relaysEnabled` | `bool` | `false` | Allow Syncthing relay fallback when direct connectivity is unavailable |
| `syncthing.compression` | `bool` | `false` | Use Syncthing `always` compression for peer connections instead of the default `metadata` mode |

**Validation:** `engine` must be `syncthing`; each `paths[]` entry must be `local:remote`.

Local ignore rules come from the synced workspace's `.stignore`. `okdev init` writes a starter `.stignore` for built-in templates, and `okdev up` creates one with default patterns if the local sync root does not already have one. Editing `.stignore` takes effect automatically as Syncthing notices the change, but it does not remove files that were already synced to the remote workspace. For faster initial syncs, consider ignoring large generated build outputs or local test artifacts such as `debug/`, `release/`, caches, and dataset directories when they do not need to exist remotely.

The `syncthing.version` field controls the local binary on your machine. The Syncthing binary inside the sidecar comes from `spec.sidecar.image`.

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
| `persistentSession` | `bool` | `true` | Enable tmux-backed interactive sessions |
| `keepAliveIntervalSeconds` | `int` | `10` | SSH keepalive interval |
| `keepAliveTimeoutSeconds` | `int` | `10` | SSH keepalive timeout |

**Validation:** both keepalive values must be `> 0`; timeout must be `>= interval`.

When `forwardAgent: true`, `okdev ssh` forwards the local `SSH_AUTH_SOCK` only for that live SSH session. This lets Git/SSH in the workload use your local agent for operations like `git push`, but it requires that your local machine already has an active ssh-agent with identities loaded via `ssh-add`.

```yaml
spec:
  ssh:
    user: root
    privateKeyPath: ~/.okdev/ssh/id_ed25519
    autoDetectPorts: true
    forwardAgent: false
    persistentSession: true
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
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

## `spec.podTemplate`

Full Kubernetes PodSpec overlay. Use this for the dev container image, resources, env vars, scheduling, labels, and extra sidecars.

- `metadata.labels` (`map[string]string`, optional)
- `spec` (Kubernetes `PodSpec`)

```yaml
spec:
  podTemplate:
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
  podTemplate:
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
  podTemplate:
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
  podTemplate:
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
      - .git/
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
  podTemplate:
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
  podTemplate:
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
    ttlHours: 168
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
  podTemplate:
    metadata:
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
      - .git/
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
  podTemplate:
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
    exclude:
      - .git/
  ports:
    - name: app
      local: 8080
      remote: 8080
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
  podTemplate:
    spec:
      containers:
        - name: dev
          image: python:3.12-slim
          command: ["sleep", "infinity"]
      nodeSelector:
        cloud.google.com/gke-nodepool: dev-pool
```
