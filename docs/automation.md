# Automation recipes

Run these Bash examples from the local repository with a selected, running
session (`okdev use <session>`). Use `--session <name>` in scripts that may run
alongside another session. Local examples require Python 3; the file verification
example also requires `sha256sum` in the dev containers.

## Verify every intended receiver

For a two-pod session with local `train.py` mapped to `/workspace/train.py`:

<!-- kind-recipe: verify -->
```bash
set -euo pipefail
results=$(mktemp)
trap 'rm -f "$results"' EXIT
okdev exec --all --json --require-all --require-sync -- sha256sum /workspace/train.py > "$results"
python3 - "$results" ./train.py 2 <<'PY'
import hashlib, json, pathlib, sys
rows = json.loads(pathlib.Path(sys.argv[1]).read_text())
expected = hashlib.sha256(pathlib.Path(sys.argv[2]).read_bytes()).hexdigest()
if len(rows) != int(sys.argv[3]) or len({r['pod'] for r in rows}) != len(rows):
    raise SystemExit('Unexpected receiver count; check the selected session/pods')
for row in rows:
    if row['status'] != 'responded' or row['exit'] != 0 or row.get('error'):
        raise SystemExit(f"Command failed on {row['pod']}: {row}")
    if row['stdout'].split()[0:1] != [expected]:
        raise SystemExit(f"Content mismatch on {row['pod']}")
PY
```

Change `2` to your intended pod count and adjust both file paths. `--all` selects
the currently discovered pods; `--require-all` alone cannot detect an expected
replica that was never discovered. File counts and byte counts do not prove that
the current content reached every receiver.

`--json` emits an array even for one pod. `--require-all` checks that all selected
pods **responded**, not that their commands succeeded: remote exit 7 is still
`status: responded` with `exit: 7`. Inspect each envelope's status, exit, and error.
Preflight failures use stderr and exit 74 (session absent) or 78 (cluster contact
failed), without a JSON result. The fail-fast shell above stops before parsing
an empty result or launching a subsequent job. Transport failures can leave the
remote outcome unknown; do not blindly replay non-idempotent commands.

## Wait for the job you just launched

This short example prints a marker and then finishes. Substitute your command,
keeping the job ID returned by that particular launch:

<!-- kind-recipe: job -->
```bash
set -euo pipefail
launch=$(okdev exec --detach -- sh -c 'printf "READY\n"; sleep 2; printf "DONE\n"')
job_id=$(printf '%s\n' "$launch" | sed -n 's/.*job_id=\([^ ]*\).*/\1/p')
test -n "$job_id"
okdev jobs wait "$job_id" --grep '^READY$'
okdev jobs wait "$job_id"
```

Use the actual `--grep` flag. A match in any selected pod's log satisfies this
wait; it does not prove every replica or an external service is ready. A marker
from an older job cannot satisfy a wait on this new ID. A terminal job that never
printed the marker fails the grep wait. Plain `jobs wait` succeeds only when all
selected job records finish successfully.

For long-running monitoring, launch the monitor with `okdev exec --detach --
sh -c 'while :; do date; sleep 30; done'`, keep its new ID, read `okdev jobs logs
<id> --tail 20`, and finish with `okdev jobs stop <id>`. Do not add `nohup` or a
second background `&` inside the detached command.

## Keep shell expansion on the intended machine

<!-- kind-recipe: quoting -->
```bash
okdev exec -- sh -c 'printf "%s\n" "$HOSTNAME"'
```

Single quotes let the remote shell expand `$HOSTNAME`; double quotes would let
your local shell expand it first. For complex commands, write a local script
and use `okdev exec --all --script ./probe.sh -- 'argument with spaces'`. Use a
quoted heredoc (`<<'SH'`) when creating that script to preserve remote variables.

## Choose a local bind address

<!-- kind-recipe: forward -->
```bash
okdev port-forward --address 127.0.0.1 "${local_port:-18080}:${remote_port:-8080}"
```

Omitting `--address` keeps the existing `localhost` default. To deliberately
accept connections on all IPv4 interfaces, use `--address 0.0.0.0`; this makes
the forwarded service reachable through those interfaces. The foreground
command runs until interrupted.

## Exclude metadata before the initial sync

When remote tools do not need Git history, add `.git` to the synced root's
`.stignore` before `okdev up`. Review the starter rules already created by
`okdev init`, preserving rules you need. Add generated outputs or local datasets
only when they are not required remotely. Ignore changes do not delete files
that were already transferred. See [sync configuration](config-manifest.md)
and the [command reference](command-reference.md) for detailed semantics.
