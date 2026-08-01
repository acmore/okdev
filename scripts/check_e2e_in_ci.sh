#!/usr/bin/env bash
set -euo pipefail

# Every kind scenario the local suite runs must also run in CI.
#
# Adding a scenario to scripts/e2e_local_kind.sh and forgetting the workflow is
# easy and silent: the suite goes green locally while CI never exercises the
# path. That happened to okdev migrate and the volumes injection, both of which
# ran only on macOS until this check existed.

cd "$(dirname "$0")/.."

local_list=$(grep -oE "scripts/e2e_[a-z_]*\.sh" scripts/e2e_local_kind.sh | sort -u)
ci_list=$(grep -oE "scripts/e2e_[a-z_]*\.sh" .github/workflows/e2e-kind.yml | sort -u)

# Opt-in scenarios need external inputs CI does not have.
optional="scripts/e2e_local_large_repo.sh"

missing=$(comm -23 <(echo "$local_list") <(echo "$ci_list") | grep -vxF "$optional" || true)

if [[ -n "$missing" ]]; then
  echo "These scenarios run locally but not in CI:" >&2
  echo "$missing" | sed 's/^/  /' >&2
  echo >&2
  echo "Add each to .github/workflows/e2e-kind.yml, or list it as optional here." >&2
  exit 1
fi

echo "every local kind scenario runs in CI"
