#!/usr/bin/env bash

# E2E scripts often capture stdout from okdev subcommands and compare exact
# command output. Suppress non-interactive "Using config: ..." announcements
# so those captures only contain the command result.
export OKDEV_QUIET_CONFIG_ANNOUNCE=1

make_workdir() {
  local root="${OKDEV_TMPDIR:-${TMPDIR:-/tmp}}"
  mkdir -p "$root"
  mktemp -d "$root/okdev.XXXXXX"
}

replace_all_in_file() {
  local file="$1"
  local old="$2"
  local new="$3"
  python3 - "$file" "$old" "$new" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
path.write_text(text.replace(old, new))
PY
}

replace_first_in_file() {
  local file="$1"
  local old="$2"
  local new="$3"
  python3 - "$file" "$old" "$new" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
old = sys.argv[2]
new = sys.argv[3]
text = path.read_text()
idx = text.find(old)
if idx == -1:
    raise SystemExit(0)
path.write_text(text[:idx] + new + text[idx + len(old):])
PY
}

insert_after_line_once() {
  local file="$1"
  local anchor="$2"
  local line="$3"
  python3 - "$file" "$anchor" "$line" <<'PY'
import pathlib, sys
path = pathlib.Path(sys.argv[1])
anchor = sys.argv[2]
line = sys.argv[3]
text = path.read_text()
if line in text:
    raise SystemExit(0)
needle = anchor + "\n"
if needle not in text:
    raise SystemExit(f"anchor not found: {anchor}")
path.write_text(text.replace(needle, needle + line + "\n", 1))
PY
}

process_commands_containing() {
	local needle="$1"
	local snapshot
	snapshot="$(mktemp)"
	ps ax -o pid= -o command= >"$snapshot"
	python3 -c '
import sys

needle = sys.argv[1]
path = sys.argv[2]
with open(path) as f:
    lines = f.readlines()
for line in lines:
    if needle in line:
        print(line, end="")
' "$needle" "$snapshot"
	rm -f "$snapshot"
}

process_commands_for_session_sync() {
	local session_name="$1"
	local snapshot
	snapshot="$(mktemp)"
	ps ax -o pid= -o command= >"$snapshot"
	python3 -c '
import sys

session_name = sys.argv[1]
path = sys.argv[2]
with open(path) as f:
    lines = f.readlines()
for line in lines:
    if session_name in line and " sync" in line:
        print(line, end="")
' "$session_name" "$snapshot"
	rm -f "$snapshot"
}

assert_no_local_sync_processes() {
  local session_name="$1"
  local sync_home="$2"
  local matches=""

  for _ in $(seq 1 20); do
    matches="$(process_commands_containing "$sync_home"; process_commands_for_session_sync "$session_name")"
    if [[ -z "$matches" ]]; then
      return 0
    fi
    sleep 0.25
  done

  echo "ERROR: expected no local sync processes for session $session_name" >&2
  echo "$matches" >&2
  return 1
}

sync_pid_from_status() {
  grep -oE 'background: running \(pid [0-9]+\)' | grep -oE '[0-9]+' | head -1
}

session_managed_pod_selector() {
  local session_name="$1"
  printf 'okdev.io/managed=true,okdev.io/session=%s' "$session_name"
}

session_attachable_pod_name() {
  local namespace="$1"
  local session_name="$2"
  kubectl -n "$namespace" get pods -l "$(session_managed_pod_selector "$session_name")" -o json |
    python3 -c 'import json, sys
data = json.load(sys.stdin)
items = data.get("items", [])
attachable = [item for item in items if item.get("metadata", {}).get("labels", {}).get("okdev.io/attachable", "").strip().lower() == "true"]
selected = attachable or items
if selected:
    print(selected[0].get("metadata", {}).get("name", ""))'
}

session_attachable_pod_names() {
  local namespace="$1"
  local session_name="$2"
  kubectl -n "$namespace" get pods -l "$(session_managed_pod_selector "$session_name")" -o json |
    python3 -c 'import json, sys
data = json.load(sys.stdin)
items = data.get("items", [])
attachable = [item for item in items if item.get("metadata", {}).get("labels", {}).get("okdev.io/attachable", "").strip().lower() == "true"]
selected = attachable or items
print(" ".join(item.get("metadata", {}).get("name", "") for item in selected if item.get("metadata", {}).get("name")))' 
}
