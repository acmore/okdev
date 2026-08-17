#!/usr/bin/env bash
set -euo pipefail

. "$(dirname "$0")/e2e_lib.sh"

OKDEV_BIN="${OKDEV_BIN:-$(pwd)/bin/okdev}"
WORKDIR="$(make_workdir)"
PROJECT_DIR="$WORKDIR/project"
HOME_DIR="$WORKDIR/home"
CFG_PATH="$PROJECT_DIR/.okdev/okdev.yaml"

cleanup() {
  status=$?
  rm -rf "$WORKDIR"
  return "$status"
}
trap cleanup EXIT

mkdir -p "$PROJECT_DIR/.okdev/templates/manifests" "$PROJECT_DIR/src/pkg" "$HOME_DIR"
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
files:
  - path: .okdev/pod.yaml
    template: manifests/pod.yaml.tmpl
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
  workload:
    type: pod
    manifestPath: pod.yaml
EOF

cat >"$PROJECT_DIR/.okdev/templates/manifests/pod.yaml.tmpl" <<'EOF'
apiVersion: v1
kind: Pod
metadata:
  name: '{{`{{ .WorkloadName }}`}}'
spec:
  containers:
    - name: dev
      image: {{ .Vars.baseImage | quote }}
      command: ["sleep", "infinity"]
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
  workload:
    type: pod
    manifestPath: pod.yaml
EOF

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
    type: pytorchjob
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
  "manifestPath: pod.yaml" \
  "name: debug"; do
  if ! grep -Fq "$needle" "$CFG_PATH"; then
    echo "ERROR: expected generated config to contain: $needle" >&2
    exit 1
  fi
done

# The dev container lives in the template's companion manifest now.
TEAM_MANIFEST="$PROJECT_DIR/.okdev/pod.yaml"
if [[ ! -f "$TEAM_MANIFEST" ]]; then
  echo "ERROR: expected the template to write $TEAM_MANIFEST" >&2
  ls -la "$PROJECT_DIR/.okdev" >&2
  exit 1
fi
cat "$TEAM_MANIFEST"
if ! grep -Fq 'image: "debian:12"' "$TEAM_MANIFEST"; then
  echo "ERROR: expected the template variable to reach the manifest" >&2
  exit 1
fi

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

echo "Adding a workload from a template that declares variables"
# Additive init renders the same template the fresh path does, so it has to
# resolve the template's variables too. It rendered against an empty .Vars
# instead, which failed with a raw Go template error for any template whose
# body compared a variable — every declared variable was unreachable.
(
  cd "$PYTORCH_DIR"
  "$OKDEV_BIN" init --yes --template pytorch --workload-name extra \
    --manifest-path pytorchjob.yaml --set workerReplicas=5
)
EXTRA_MANIFEST_PATH="$PYTORCH_DIR/.okdev/extra.yaml"
if [[ ! -f "$EXTRA_MANIFEST_PATH" ]]; then
  echo "ERROR: expected additive init to scaffold $EXTRA_MANIFEST_PATH" >&2
  exit 1
fi
if ! grep -Fq "replicas: 5" "$EXTRA_MANIFEST_PATH"; then
  echo "ERROR: expected --set to reach the added workload's manifest" >&2
  cat "$EXTRA_MANIFEST_PATH" >&2
  exit 1
fi
if ! grep -Fq "name: extra" "$PYTORCH_CFG_PATH"; then
  echo "ERROR: expected the added workload to be declared in $PYTORCH_CFG_PATH" >&2
  exit 1
fi
# Adding a workload does not regenerate the project config, so the recorded
# spec.template.vars must still describe how that config was created.
if ! grep -Fq "workerReplicas: 3" "$PYTORCH_CFG_PATH"; then
  echo "ERROR: additive init must not rewrite spec.template.vars" >&2
  cat "$PYTORCH_CFG_PATH" >&2
  exit 1
fi

echo "Adding a workload without --set falls back to the declared defaults"
(
  cd "$PYTORCH_DIR"
  "$OKDEV_BIN" init --yes --template pytorch --workload-name defaulted \
    --manifest-path pytorchjob.yaml
)
DEFAULTED_MANIFEST_PATH="$PYTORCH_DIR/.okdev/defaulted.yaml"
if ! grep -Fq "replicas: 2" "$DEFAULTED_MANIFEST_PATH"; then
  echo "ERROR: expected the frontmatter default (2) in $DEFAULTED_MANIFEST_PATH" >&2
  cat "$DEFAULTED_MANIFEST_PATH" >&2
  exit 1
fi

# Adding a workload instantiates a template, so its variables are prompted for
# on a terminal. CI has none, so without --yes there is nobody to answer and it
# must refuse rather than silently take the defaults.
echo "Refusing to add a workload non-interactively without --yes"
if NOTTY_OUTPUT=$(cd "$PYTORCH_DIR" && "$OKDEV_BIN" init --template pytorch --workload-name notty \
  --manifest-path pytorchjob.yaml </dev/null 2>&1); then
  echo "ERROR: expected a refusal without a TTY and without --yes" >&2
  echo "$NOTTY_OUTPUT" >&2
  exit 1
fi
if [[ "$NOTTY_OUTPUT" != *"--yes"* || "$NOTTY_OUTPUT" != *"--set"* ]]; then
  echo "ERROR: the refusal must name the way out, got: $NOTTY_OUTPUT" >&2
  exit 1
fi
if [[ -f "$PYTORCH_DIR/.okdev/notty.yaml" ]]; then
  echo "ERROR: a refused addition must not scaffold a manifest" >&2
  exit 1
fi

echo "Rejecting a template that references an undeclared variable"
cat >"$PROJECT_DIR/.okdev/templates/undeclared.yaml.tmpl" <<'EOF'
---
name: undeclared
description: References a variable it never declares
---
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: undeclareddemo
spec:
  namespace: template-ns
  workload:
    type: pod
    manifestPath: pod.yaml
    image: {{ .Vars.neverDeclared }}
EOF
UNDECLARED_DIR="$WORKDIR/undeclared"
mkdir -p "$UNDECLARED_DIR/.okdev"
cp -R "$PROJECT_DIR/.okdev/templates" "$UNDECLARED_DIR/.okdev/templates"
if UNDECLARED_OUTPUT=$(cd "$UNDECLARED_DIR" && "$OKDEV_BIN" init --yes --template undeclared --name undeclareddemo 2>&1); then
  echo "ERROR: expected init to reject a template referencing an undeclared variable" >&2
  echo "$UNDECLARED_OUTPUT" >&2
  exit 1
fi
if [[ "$UNDECLARED_OUTPUT" != *"neverDeclared"* ]]; then
  echo "ERROR: expected the error to name the undeclared variable, got: $UNDECLARED_OUTPUT" >&2
  exit 1
fi
if [[ "$UNDECLARED_OUTPUT" == *"<no value>"* || "$UNDECLARED_OUTPUT" == *"invalid type for comparison"* ]]; then
  echo "ERROR: the raw Go template failure must not reach the user, got: $UNDECLARED_OUTPUT" >&2
  exit 1
fi

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
if ! grep -Fq "template:" "$SHADOW_DIR/.okdev/okdev.yaml"; then
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
  workload:
    type: pod
    manifestPath: pod.yaml
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
  "manifestPath: pod.yaml" \
  "namespace: custom-ns"; do
  if ! grep -Fq "$needle" "$CFG_PATH"; then
    echo "ERROR: expected migrated config to contain: $needle" >&2
    exit 1
  fi
done

echo "Template system e2e completed"
