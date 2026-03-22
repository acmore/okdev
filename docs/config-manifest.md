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

Validation:
- `ttlHours >= 0`
- `idleTimeoutMinutes >= 0`

Context precedence:
- `--context` CLI flag (highest)
- `spec.kubeContext` from manifest
- active kubeconfig current-context (default client behavior)

## `spec.volumes`

`spec.volumes` uses native Kubernetes `corev1.Volume` schema.

- Define storage source with standard `VolumeSource` (for example `emptyDir`, `persistentVolumeClaim`, `ephemeral`, `configMap`, `secret`).
- Mount points are defined with standard Kubernetes `volumeMounts` in `spec.podTemplate.spec.containers[*].volumeMounts`.

Workspace behavior:
- If a `workspace` volume is not provided, okdev injects:
  - `name: workspace`
  - `emptyDir: {}`
- okdev ensures `workspace` is mounted on:
  - `dev` container
  - `okdev-sidecar`
- Workspace mount path defaults to `/workspace` (or follows `dev` container `volumeMounts` entry for `workspace` if provided in `podTemplate`).

## `spec.sync`

- `engine` (`string`, required, currently only `syncthing`)
- `paths` (`[]string`): mappings in `local:remote` format.
- `exclude` (`[]string`): local ignore patterns.
- `remoteExclude` (`[]string`): remote-only ignore patterns (written to remote `.stignore`).
- `syncthing` (`object`):
  - `version` (`string`, default: `v1.29.7`)
  - `autoInstall` (`bool`, default: `true`)
  - `image` (`string`, default: `ghcr.io/acmore/okdev:<okdev-version>` with `edge` fallback)

Validation:
- `engine` must be `syncthing`
- each `paths[]` entry must be `local:remote`
- currently at most one `paths[]` entry

## `spec.ports`

Each item:

- `name` (`string`, optional)
- `local` (`int`, required): local listening port
- `remote` (`int`, required): remote target port in dev environment

Validation:
- `local` and `remote` must be `1..65535`
- `local` ports must be unique

## `spec.ssh`

- `user` (`string`, default: `root`)
- `remotePort` (`int`, default: `22`)
- `privateKeyPath` (`string`, optional)
- `autoDetectPorts` (`bool`, default: `true`)
- `persistentSession` (`bool`, default: `true`) enables tmux-backed interactive session mode
- `keepAliveIntervalSeconds` (`int`, default: `10`)
- `keepAliveTimeoutSeconds` (`int`, default: `10`)

Validation:
- `remotePort` must be `1..65535`
- `keepAliveIntervalSeconds > 0`
- `keepAliveTimeoutSeconds > 0`
- `keepAliveTimeoutSeconds >= keepAliveIntervalSeconds`

## `spec.lifecycle`

- `postCreate` (`string`, optional): command executed once after environment creation.
- `preStop` (`string`, optional): command executed before pod termination.

## `spec.sidecar`

- `image` (`string`, required)

Recommended:
- use release-aligned tag, for example `ghcr.io/acmore/okdev:v0.2.0`

## `spec.podTemplate`

Direct PodSpec overlay:

- `metadata.labels` (`map[string]string`, optional)
- `spec` (`k8s PodSpec`, optional)

Use this to define the dev container image, resources, extra sidecars, env vars, and scheduling settings.

## Example Manifest

```yaml
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: llm-project
spec:
  namespace: ai-dev
  session:
    defaultNameTemplate: "{{ .Repo }}-{{ .Branch }}-{{ .User }}"
    ttlHours: 72
    idleTimeoutMinutes: 120
    shareable: true
  mounts:
  volumes:
    - name: workspace
      persistentVolumeClaim:
        claimName: team-workspace
    - name: datasets
      persistentVolumeClaim:
        claimName: team-datasets
  sync:
    engine: syncthing
    syncthing:
      version: v1.29.7
      autoInstall: true
    paths:
      - .:/workspace
    exclude:
      - .git/
      - .venv/
      - node_modules/
      - checkpoints/
    remoteExclude:
      - ".cache/"
  ports:
    - name: app
      local: 8080
      remote: 8080
    - name: tensorboard
      local: 6006
      remote: 6006
  ssh:
    user: root
    remotePort: 22
    persistentSession: true
    keepAliveIntervalSeconds: 30
    keepAliveTimeoutSeconds: 90
  sidecar:
    image: ghcr.io/acmore/okdev:v0.2.0
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
