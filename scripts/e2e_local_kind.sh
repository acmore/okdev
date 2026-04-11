#!/usr/bin/env bash
set -euo pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$ROOT_DIR"

CLUSTER_NAME="${CLUSTER_NAME:-okdev-e2e}"
OKDEV_BIN="${OKDEV_BIN:-$ROOT_DIR/bin/okdev}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-okdev-sidecar:v0.0.0-e2e}"
RUN_PYTORCHJOB="${RUN_PYTORCHJOB:-0}"
RUN_SYNC_HEALTH="${RUN_SYNC_HEALTH:-1}"
RUN_LARGE_REPO="${RUN_LARGE_REPO:-0}"
LARGE_REPO_PATH="${LARGE_REPO_PATH:-}"
LARGE_REPO_URL="${LARGE_REPO_URL:-}"
LARGE_REPO_REF="${LARGE_REPO_REF:-}"
KEEP_CLUSTER="${KEEP_CLUSTER:-1}"
RUNNER_KUBECONFIG="${RUNNER_KUBECONFIG:-$(mktemp)}"
RUNNER_TMPDIR="${RUNNER_TMPDIR:-/tmp/okdev-e2e}"

if ! command -v kind >/dev/null 2>&1; then
  echo "kind is required" >&2
  exit 1
fi

if ! command -v kubectl >/dev/null 2>&1; then
  echo "kubectl is required" >&2
  exit 1
fi

cleanup() {
  status=$?
  rm -f "$RUNNER_KUBECONFIG"
  if [[ "$KEEP_CLUSTER" != "1" ]]; then
    kind delete cluster --name "$CLUSTER_NAME" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT

if ! kind get clusters 2>/dev/null | grep -Fxq "$CLUSTER_NAME"; then
  echo "Creating kind cluster: $CLUSTER_NAME"
  kind create cluster --name "$CLUSTER_NAME"
else
  echo "Reusing kind cluster: $CLUSTER_NAME"
fi

kind export kubeconfig --name "$CLUSTER_NAME" --kubeconfig "$RUNNER_KUBECONFIG" >/dev/null
export KUBECONFIG="$RUNNER_KUBECONFIG"
mkdir -p "$RUNNER_TMPDIR"
export OKDEV_TMPDIR="$RUNNER_TMPDIR"

echo "Building okdev CLI"
go build -o "$OKDEV_BIN" ./cmd/okdev

echo "Building sidecar image"
docker build -f infra/sidecar/Dockerfile -t "$SIDECAR_IMAGE" .

echo "Loading sidecar image into kind"
kind load docker-image "$SIDECAR_IMAGE" --name "$CLUSTER_NAME"

echo "Running local Kind e2e suite"
bash scripts/e2e_template_system.sh
bash scripts/e2e_kind_smoke.sh
bash scripts/e2e_kind_deployment.sh
bash scripts/e2e_kind_job.sh
bash scripts/e2e_kind_multi_session.sh

if [[ "$RUN_SYNC_HEALTH" == "1" ]]; then
  bash scripts/e2e_kind_sync_health.sh
fi

if [[ "$RUN_PYTORCHJOB" == "1" ]]; then
  echo "Installing Kubeflow Training Operator"
  kubectl apply --server-side -k "https://github.com/kubeflow/training-operator.git/manifests/overlays/standalone?ref=v1.8.1"
  kubectl -n kubeflow wait --for=condition=Available deployment/training-operator --timeout=120s
  bash scripts/e2e_kind_pytorchjob.sh
fi

if [[ "$RUN_LARGE_REPO" == "1" ]]; then
  if [[ -z "$LARGE_REPO_PATH" && -z "$LARGE_REPO_URL" ]]; then
    echo "set LARGE_REPO_PATH or LARGE_REPO_URL when RUN_LARGE_REPO=1" >&2
    exit 1
  fi
  LARGE_REPO_PATH="$LARGE_REPO_PATH" LARGE_REPO_URL="$LARGE_REPO_URL" LARGE_REPO_REF="$LARGE_REPO_REF" \
    OKDEV_BIN="$OKDEV_BIN" SIDECAR_IMAGE="$SIDECAR_IMAGE" \
    bash scripts/e2e_local_large_repo.sh
fi

echo "Local Kind e2e suite completed"
