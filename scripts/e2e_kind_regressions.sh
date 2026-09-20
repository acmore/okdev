#!/usr/bin/env bash
set -euo pipefail
ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
# Optional names allow a PR's scenarios to run against its exact CLI build.
if [[ $# -gt 0 ]]; then
  for scenario in "$@"; do
    python3 -B "$ROOT_DIR/scripts/e2e_kind_regression_${scenario}.py"
  done
else
  for scenario in "$ROOT_DIR"/scripts/e2e_kind_regression_*.py; do
    python3 -B "$scenario"
  done
fi
