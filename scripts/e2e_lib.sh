#!/usr/bin/env bash

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
