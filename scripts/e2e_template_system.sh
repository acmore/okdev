#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
WORKDIR="$(make_workdir)"
PROJECT_DIR="$WORKDIR/project"
HOME_DIR="$WORKDIR/home"
CFG_PATH="$PROJECT_DIR/.okdev.yaml"

cleanup() {
  status=$?
  rm -rf "$WORKDIR"
  return "$status"
}
trap cleanup EXIT

mkdir -p "$PROJECT_DIR/.okdev/templates" "$PROJECT_DIR/src/pkg" "$HOME_DIR"
export HOME="$HOME_DIR"

# Seed a config file so template list/show from nested directories can resolve
# the project root before init rewrites the real config.
printf 'apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: template-discovery\nspec: {}\n' >"$CFG_PATH"

cat >"$PROJECT_DIR/.okdev/templates/team.yaml.tmpl" <<'EOF'
---
name: team
description: Team template
variables:
  - name: baseImage
    description: Base image
    type: string
    default: ubuntu:22.04
  - name: exposeDebug
    description: Expose debug port
    type: bool
    default: false
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name | upper | lower }}
spec:
  namespace: {{ .Namespace }}
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  ports:
{{- if .Vars.exposeDebug }}
    - name: debug
      local: 5678
      remote: 5678
{{- end }}
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .Vars.baseImage | quote }}
EOF

cat >"$PROJECT_DIR/.okdev/templates/basic.yaml.tmpl" <<'EOF'
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  podTemplate:
    spec:
      containers:
        - name: dev
          image: ubuntu:22.04
EOF

mkdir -p "$PROJECT_DIR/.okdev/templates/manifests"
cat >"$PROJECT_DIR/.okdev/templates/pytorch.yaml.tmpl" <<'EOF'
---
name: pytorch
description: PyTorchJob template
variables:
  - name: trainImage
    description: Training image
    type: string
    default: pytorch/pytorch:2.7.0-cuda12.8-cudnn9-runtime
  - name: workerReplicas
    description: Worker replica count
    type: int
    default: 2
files:
  - path: "{{ .ManifestPath }}"
    template: manifests/pytorchjob.yaml.tmpl
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: {{ .Name }}
spec:
  namespace: {{ .Namespace }}
  sync:
    paths:
      - "{{ .SyncLocal }}:{{ .SyncRemote }}"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  workload:
    type: {{ .WorkloadType }}
    manifestPath: {{ .ManifestPath }}
    inject:
      - path: "spec.pytorchReplicaSpecs.Master.template"
      - path: "spec.pytorchReplicaSpecs.Worker.template"
    attach:
      container: dev
EOF

cat >"$PROJECT_DIR/.okdev/templates/manifests/pytorchjob.yaml.tmpl" <<'EOF'
apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: {{`{{ .WorkloadName }}`}}
spec:
  pytorchReplicaSpecs:
    Master:
      replicas: 1
      restartPolicy: Never
      template:
        spec:
          containers:
            - name: dev
              image: {{ .Vars.trainImage }}
              command: ["sleep", "infinity"]
              volumeMounts:
                - name: workspace
                  mountPath: {{ .SyncRemote }}
    Worker:
      replicas: {{ .Vars.workerReplicas }}
      restartPolicy: Never
      template:
        spec:
          containers:
            - name: dev
              image: {{ .Vars.trainImage }}
              command: ["sleep", "infinity"]
              volumeMounts:
                - name: workspace
                  mountPath: {{ .SyncRemote }}
EOF

echo "Listing project templates from a nested directory"
LIST_OUTPUT=$(cd "$PROJECT_DIR/src/pkg" && "$OKDEV_BIN" template list)
echo "$LIST_OUTPUT"
if [[ "$LIST_OUTPUT" != *"team"* || "$LIST_OUTPUT" != *"project"* ]]; then
  echo "ERROR: expected project template in nested template list output" >&2
  exit 1
fi

echo "Showing template metadata"
SHOW_OUTPUT=$(cd "$PROJECT_DIR/src/pkg" && "$OKDEV_BIN" template show team)
echo "$SHOW_OUTPUT"
if [[ "$SHOW_OUTPUT" != *"Team template"* || "$SHOW_OUTPUT" != *"baseImage"* || "$SHOW_OUTPUT" != *"bool"* ]]; then
  echo "ERROR: expected frontmatter metadata in template show output" >&2
  exit 1
fi

echo "Showing template companion files"
PYTORCH_SHOW_OUTPUT=$(cd "$PROJECT_DIR/src/pkg" && "$OKDEV_BIN" template show pytorch)
echo "$PYTORCH_SHOW_OUTPUT"
if [[ "$PYTORCH_SHOW_OUTPUT" != *"PyTorchJob template"* || "$PYTORCH_SHOW_OUTPUT" != *"Files:"* || "$PYTORCH_SHOW_OUTPUT" != *"manifests/pytorchjob.yaml.tmpl"* ]]; then
  echo "ERROR: expected companion file metadata in template show output" >&2
  exit 1
fi
rm -f "$CFG_PATH"

echo "Initializing from project template with custom vars"
(
  cd "$PROJECT_DIR"
  "$OKDEV_BIN" init \
    --yes \
    --template team \
    --name templatedemo \
    --set baseImage=debian:12 \
    --set exposeDebug=true
)

if [[ ! -f "$CFG_PATH" ]]; then
  echo "ERROR: expected init to write $CFG_PATH" >&2
  exit 1
fi
cat "$CFG_PATH"
for needle in \
  "template:" \
  "name: team" \
  "baseImage: debian:12" \
  "exposeDebug: true" \
  "image: \"debian:12\"" \
  "name: debug"; do
  if ! grep -Fq "$needle" "$CFG_PATH"; then
    echo "ERROR: expected generated config to contain: $needle" >&2
    exit 1
  fi
done

echo "Initializing PyTorchJob template with companion manifest"
PYTORCH_DIR="$WORKDIR/pytorch"
mkdir -p "$PYTORCH_DIR/.okdev"
cp -R "$PROJECT_DIR/.okdev/templates" "$PYTORCH_DIR/.okdev/templates"
(
  cd "$PYTORCH_DIR"
  "$OKDEV_BIN" init \
    --yes \
    --template pytorch \
    --name torchdemo \
    --workload pytorchjob \
    --manifest-path pytorchjob.yaml \
    --sync-remote /train \
    --set trainImage=example.com/train:latest \
    --set workerReplicas=3
)

PYTORCH_CFG_PATH="$PYTORCH_DIR/.okdev/okdev.yaml"
PYTORCH_MANIFEST_PATH="$PYTORCH_DIR/.okdev/pytorchjob.yaml"

if [[ ! -f "$PYTORCH_CFG_PATH" ]]; then
  echo "ERROR: expected PyTorch template to write config to $PYTORCH_CFG_PATH" >&2
  exit 1
fi
if [[ ! -f "$PYTORCH_MANIFEST_PATH" ]]; then
  echo "ERROR: expected PyTorch template to write companion manifest to $PYTORCH_MANIFEST_PATH" >&2
  exit 1
fi
cat "$PYTORCH_CFG_PATH"
cat "$PYTORCH_MANIFEST_PATH"
for needle in \
  "template:" \
  "name: pytorch" \
  "trainImage: example.com/train:latest" \
  "workerReplicas: 3" \
  "type: pytorchjob" \
  "manifestPath: pytorchjob.yaml"; do
  if ! grep -Fq "$needle" "$PYTORCH_CFG_PATH"; then
    echo "ERROR: expected PyTorch config to contain: $needle" >&2
    exit 1
  fi
done
for needle in \
  "kind: PyTorchJob" \
  "name: {{ .WorkloadName }}" \
  "replicas: 3" \
  "image: example.com/train:latest" \
  "mountPath: /train"; do
  if ! grep -Fq "$needle" "$PYTORCH_MANIFEST_PATH"; then
    echo "ERROR: expected PyTorch manifest to contain: $needle" >&2
    exit 1
  fi
done

echo "Checking shadowed basic template behavior"
SHADOW_DIR="$WORKDIR/shadow"
mkdir -p "$SHADOW_DIR/.okdev/templates"
cp "$PROJECT_DIR/.okdev/templates/basic.yaml.tmpl" "$SHADOW_DIR/.okdev/templates/basic.yaml.tmpl"
(
  cd "$SHADOW_DIR"
  "$OKDEV_BIN" init --yes --template basic --name shadowdemo
)
if [[ -f "$SHADOW_DIR/.stignore" ]]; then
  echo "ERROR: shadowed basic should not receive built-in .stignore" >&2
  exit 1
fi
if ! grep -Fq "template:" "$SHADOW_DIR/.okdev.yaml"; then
  echo "ERROR: shadowed basic should persist spec.template" >&2
  exit 1
fi

echo "Migrating template with required variable"
cat >"$PROJECT_DIR/.okdev/templates/required.yaml.tmpl" <<'EOF'
---
name: required
description: Required var template
variables:
  - name: requiredImage
    description: Required image
    type: string
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: templatedemo
spec:
  namespace: template-ns
  sync:
    paths:
      - ".:/workspace"
  ssh:
    user: root
  sidecar:
    image: ghcr.io/acmore/okdev:edge
  podTemplate:
    spec:
      containers:
        - name: dev
          image: {{ .Vars.requiredImage }}
EOF

replace_all_in_file "$CFG_PATH" "namespace: default" "namespace: custom-ns"
if MISSING_OUTPUT=$(cd "$PROJECT_DIR" && "$OKDEV_BIN" migrate --template required --yes --no-backup 2>&1); then
  echo "ERROR: expected migrate to reject missing required variable" >&2
  exit 1
fi
echo "$MISSING_OUTPUT"
if [[ "$MISSING_OUTPUT" != *"variable \"requiredImage\" is required"* ]]; then
  echo "ERROR: expected missing required variable error" >&2
  exit 1
fi
MIGRATE_OUTPUT=$(cd "$PROJECT_DIR" && "$OKDEV_BIN" migrate --template required --set requiredImage=alpine:3.20 --yes --no-backup)
echo "$MIGRATE_OUTPUT"
for needle in \
  "name: required" \
  "requiredImage: alpine:3.20" \
  "namespace: custom-ns"; do
  if ! grep -Fq "$needle" "$CFG_PATH"; then
    echo "ERROR: expected migrated config to contain: $needle" >&2
    exit 1
  fi
done

echo "Template system e2e completed"
