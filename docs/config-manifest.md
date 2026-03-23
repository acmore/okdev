# Config Manifest

`okdev` is configured by a single manifest (default: `.okdev.yaml`).

## Top-Level

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: my-project
spec: {}
```

## Field Reference

### `apiVersion` and `kind`

- `apiVersion`: must be `okdev.io/v1alpha1`
- `kind`: must be `DevEnvironment`

### `metadata`

- `metadata.name` (`string`, required): logical project/env name.

### `spec`

- `spec.namespace` (`string`, default: `default`)
- `spec.kubeContext` (`string`, optional): kubeconfig context used by okdev commands.
- `spec.session` (`object`)
- `spec.volumes` (`array`, optional)
- `spec.sync` (`object`)
- `spec.ports` (`array`)
- `spec.ssh` (`object`)
- `spec.lifecycle` (`object`)
- `spec.sidecar` (`object`)
- `spec.podTemplate` (`object`)

## `spec.session`

- `defaultNameTemplate` (`string`): template for inferred session name.
- `ttlHours` (`int`, default from template): reserved for lifecycle policy.
- `idleTimeoutMinutes` (`int`, default from template): reserved for lifecycle policy.
- `shareable` (`bool`): marks intent for team sharing workflows.

### Example

```yaml
spec:
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
```

### Validation

- `ttlHours >= 0`
- `idleTimeoutMinutes >= 0`

### Context Precedence

- `--context` CLI flag (highest)
- `spec.kubeContext` from manifest
- active kubeconfig current-context (default client behavior)

## `spec.volumes`

`spec.volumes` uses native Kubernetes `corev1.Volume` schema.

- Define storage source with standard `VolumeSource` (for example `emptyDir`, `persistentVolumeClaim`, `ephemeral`, `configMap`, `secret`).
- Mount points are defined with standard Kubernetes `volumeMounts` in `spec.podTemplate.spec.containers[*].volumeMounts`.

### Example

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

### More Examples

#### `emptyDir` Scratch Space

Useful for build caches, temporary outputs, or other pod-lifetime data.

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

#### `configMap` and `secret` Mounts

Useful when the dev container needs checked-in config plus cluster-managed credentials.

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

#### `ephemeral` Per-Session Persistent Storage

Useful when each okdev session should get its own PVC-backed storage that is created with the pod and removed with it.

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

### Workspace Behavior

- If a `workspace` volume is not provided, okdev injects a volume with `name: workspace` and `emptyDir: {}`.
- okdev ensures `workspace` is mounted on the `dev` container and `okdev-sidecar`.
- Workspace mount path defaults to `/workspace` (or follows `dev` container `volumeMounts` entry for `workspace` if provided in `podTemplate`).

## `spec.sync`

- `engine` (`string`, required, currently only `syncthing`)
- `paths` (`[]string`): mappings in `local:remote` format.
- `exclude` (`[]string`): local ignore patterns.
- `remoteExclude` (`[]string`): remote-only ignore patterns (written to remote `.stignore`).
- `syncthing` (`object`): includes `version` (`string`, default: `v1.29.7`) for the local auto-installed Syncthing client, `autoInstall` (`bool`, default: `true`), and `image` (`string`, default: `ghcr.io/acmore/okdev:<okdev-version>` with `edge` fallback) for the sidecar runtime image.

### Example

```yaml
spec:
  sync:
    engine: syncthing
    syncthing:
      version: v1.29.7
      autoInstall: true
    paths:
      - .:/workspace
    exclude:
      - .git/
      - node_modules/
      - .venv/
    remoteExclude:
      - ".cache/"
```

### Validation

- `engine` must be `syncthing`
- each `paths[]` entry must be `local:remote`
- currently at most one `paths[]` entry

### Version Behavior

- `spec.sync.syncthing.version` controls the local Syncthing binary that `okdev` installs or reuses on your machine.
- The Syncthing binary inside the pod sidecar comes from `spec.sidecar.image`, not from `spec.sync.syncthing.version`.

## `spec.ports`

### Each Item

- `name` (`string`, optional)
- `local` (`int`, required): local listening port
- `remote` (`int`, required): remote target port in dev environment

### Example

```yaml
spec:
  ports:
    - name: app
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
```

### Validation

- `local` and `remote` must be `1..65535`
- `local` ports must be unique

## `spec.ssh`

- `user` (`string`, default: `root`)
- `privateKeyPath` (`string`, optional)
- `autoDetectPorts` (`bool`, default: `true`)
- `persistentSession` (`bool`, default: `true`) enables tmux-backed interactive session mode
- `keepAliveIntervalSeconds` (`int`, default: `10`)
- `keepAliveTimeoutSeconds` (`int`, default: `10`)

### Example

```yaml
spec:
  ssh:
    user: root
    privateKeyPath: ~/.okdev/ssh/id_ed25519
    autoDetectPorts: true
    persistentSession: true
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
```

### Validation

- `keepAliveIntervalSeconds > 0`
- `keepAliveTimeoutSeconds > 0`
- `keepAliveTimeoutSeconds >= keepAliveIntervalSeconds`

## `spec.lifecycle`

- `postCreate` (`string`, optional): command executed once after environment creation.
- `preStop` (`string`, optional): command executed before pod termination.

### Example

```yaml
spec:
  lifecycle:
    postCreate: uv sync --dev
    preStop: pkill -f my-dev-server || true
```

## `spec.sidecar`

- `image` (`string`, required)

### Example

```yaml
spec:
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.16
```

### Recommended

- use release-aligned tag, for example `ghcr.io/acmore/okdev:v0.2.0`

## `spec.podTemplate`

### Direct PodSpec Overlay

- `metadata.labels` (`map[string]string`, optional)
- `spec` (`k8s PodSpec`, optional)

Use this to define the dev container image, resources, extra sidecars, env vars, and scheduling settings.

### Example

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

## Examples

### Minimal -- Web app with defaults

The simplest useful manifest. Uses an ephemeral workspace (emptyDir), syncs the current directory, and forwards one port.

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

### Persistent workspace with PVC

Workspace data survives pod restarts. Useful when `npm install` or `pip install` takes a long time and you don't want to redo it on every `okdev up`.

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

Single GPU development with a persistent workspace and a dataset volume mounted read-only.

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
    exclude:
      - .git/
      - .venv/
      - __pycache__/
      - wandb/
    remoteExclude:
      - ".cache/"
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

### Multi-GPU LLM fine-tuning

Multiple GPUs, large memory, team-shared session naming, and a longer keepalive for stable long-running connections.

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-finetune
spec:
  namespace: ai-team
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 168
    shareable: true
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
    exclude:
      - .git/
      - checkpoints/
      - "*.safetensors"
    remoteExclude:
      - ".cache/"
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

### Go microservice with no tmux

Lightweight setup for a Go service. Disables tmux for a plain SSH shell experience.

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

### Custom kube context and namespace

Explicit cluster targeting for multi-cluster setups. Useful when your kubeconfig has multiple contexts.

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
