#!/usr/bin/env bash
set -euo pipefail

go_files="$(git ls-files '*.go')"

if [[ -z "$go_files" ]]; then
  exit 0
fi

unformatted="$(printf '%s\n' "$go_files" | xargs gofmt -l)"

if [[ -z "$unformatted" ]]; then
  exit 0
fi

printf 'gofmt check failed. Run:\n'
printf '  gofmt -w'
while IFS= read -r file; do
  printf ' %q' "$file"
done <<< "$unformatted"
printf '\n\nUnformatted files:\n'
printf '%s\n' "$unformatted" | sed 's/^/  /'
exit 1
