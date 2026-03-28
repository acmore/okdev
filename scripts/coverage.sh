#!/usr/bin/env sh
set -eu

ROOT_DIR="$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"
cd "$ROOT_DIR"

if [ "$#" -eq 0 ]; then
  set -- ./...
fi

packages="$(go list -f '{{if or .TestGoFiles .XTestGoFiles}}{{.ImportPath}}{{end}}' "$@" | sed '/^$/d')"
if [ -z "$packages" ]; then
  echo "no packages with tests found" >&2
  exit 0
fi

tmpdir="$(mktemp -d)"
cleanup() {
  rm -rf "$tmpdir"
}
trap cleanup EXIT INT TERM

merged="$tmpdir/coverage.out"
printf 'mode: count\n' >"$merged"

index=0
status=0
for pkg in $packages; do
  profile="$tmpdir/$index.out"
  if ! go test "$pkg" -coverprofile="$profile" -covermode=count; then
    status=1
    continue
  fi
  if [ -s "$profile" ]; then
    tail -n +2 "$profile" >>"$merged"
  fi
  index=$((index + 1))
done

if [ "$status" -ne 0 ]; then
  exit "$status"
fi

echo
echo "Total coverage:"
go tool cover -func="$merged" | tail -n 1
