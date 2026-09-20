# Distribute a snapshot when sync is unavailable

`okdev cp` works independently of Syncthing. Its default remains the target pod
only; `--pod` explicitly selects receivers and `--all` explicitly selects all
discovered session pods. Copy completion is reported per pod, and any failed
transfer makes the command exit nonzero.

Use a fresh destination outside managed sync roots. This avoids an unhealthy or
later-recovered sync channel overwriting the delivered snapshot. Snapshot delivery
does **not** restart continuous synchronization: later edits require another
snapshot and distribution. Do not add `--require-sync` to this fallback.

## Deliver and verify the same archive on selected pods

Select the session with `okdev use <session>` (or pass `--session` explicitly).
Read the receiver names from `okdev status`, then set `pod_a` and `pod_b` to the
two intended pods. Put the desired files in `./snapshot-source` and stop editing
that directory while building the archive. It is copied as-is, including hidden
files; `.stignore` does not filter `cp` or the archive, so omit metadata, secrets,
and generated files you do not intend to distribute from this staging directory.

The local machine needs Python 3. The dev containers need `sh`, `tar`, and
`sha256sum`; the recipe checks these before copying. It does not use `rsync`,
remote Python, or an external local archive program.

<!-- kind-recipe: snapshot -->
```bash
set -euo pipefail
targets=(--pod "${pod_a:?set pod_a}" --pod "${pod_b:?set pod_b}")
okdev exec "${targets[@]}" -- sh -c 'command -v tar >/dev/null && command -v sha256sum >/dev/null'
bundle=$(mktemp)
trap 'rm -f "$bundle"' EXIT
python3 - "$bundle" <<'PY'
import sys, tarfile
with tarfile.open(sys.argv[1], 'w') as archive:
    archive.add('./snapshot-source', arcname='.')
PY
digest=$(python3 - "$bundle" <<'PY'
import hashlib, sys
with open(sys.argv[1], 'rb') as source:
    digest = hashlib.sha256()
    for block in iter(lambda: source.read(1024 * 1024), b''):
        digest.update(block)
    print(digest.hexdigest())
PY
)
snapshot_dir="/tmp/okdev-snapshot-$(python3 -c 'import uuid; print(uuid.uuid4().hex)')"
remote_archive="$snapshot_dir.tar"
okdev cp "${targets[@]}" "$bundle" ":$remote_archive"
okdev exec "${targets[@]}" --no-prefix=false -- sh -c \
  'printf "%s  %s\n" "$1" "$2" | sha256sum -c -' sh "$digest" "$remote_archive"
okdev exec "${targets[@]}" -- sh -c \
  'mkdir "$1" && tar -xf "$2" -C "$1"' sh "$snapshot_dir" "$remote_archive"
printf 'snapshot_path=%s\narchive_path=%s\n' "$snapshot_dir" "$remote_archive"
```

The archive hash check runs on every selected receiver before any extraction. Each extraction
must also succeed. If a command fails, stop and inspect the pod named in the
output; successful transfers on other pods are not rolled back. Keep the printed
paths for this delivery. Run code from `snapshot_path`, for example:

```bash
okdev exec --pod "$pod_a" -- sh -c 'cd "$1" && exec ./run.sh' sh "$snapshot_dir"
```

To target every discovered session pod, replace the `targets` assignment with
`targets=(--all)` after checking the expected replica count. Explicit pod names
are preferable when you have a fixed receiver list: a missing name fails instead
of silently reducing that list. Do not use `--ready-only` to skip an intended
receiver. `cp --verify` is for **downloads**; uploads use the explicit remote
SHA-256 check above.

When finished, remove only the paths created for this delivery, on the same
selected pods:

```bash
okdev exec "${targets[@]}" -- rm -f -- "$remote_archive"
okdev exec "${targets[@]}" -- rm -rf -- "$snapshot_dir"
```

## If there is already a shared volume

If every receiver genuinely mounts the same writable shared volume, copy and
extract once through one explicitly selected writer pod into a fresh directory
on that volume. Then verify a known file's hash from every intended reader, as
in the [automation recipe](automation.md#verify-every-intended-receiver). Do not
fan out concurrent writes to the same shared path. Identical mount paths backed
by separate `emptyDir` volumes are not shared storage.
