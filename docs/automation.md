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

## Bounded execution preflight retries

`okdev exec --preflight-retry-timeout 30s -- <command>` gives the read-only
session access/ownership Pod-list check a total retry budget. Temporary cluster
contact failures back off from 750 ms to at most 5 seconds; each request and
sleep respects the budget and caller cancellation/deadline. Zero (the default)
keeps the existing two attempts. Negative values are rejected. RBAC/authentication
errors and a reachable-but-absent session are not retried.

This budget starts after config/session resolution and ends before target
selection, uploads, sync gates, or command delivery. Use an explicit session in
automation when session inference might itself require cluster discovery.
`--timeout` remains a per-pod command timeout, separate from this preflight budget.
Retry notices go to stderr. Budget exhaustion exits 78 with empty stdout, even
with `--json`; cancellation follows the caller context. A persistent outage
still warrants checking connectivity rather than repeatedly restarting budgets.

User commands, uploaded scripts and detach launch requests are sent once. A
stream failure after delivery may mean the command already ran or is still
running: do not blindly retry a non-idempotent command. A remote nonzero exit is
also never retried. This does not provide exactly-once execution across a lost
connection. Check job IDs, logs and launch-specific markers before deciding how
to recover an ambiguous detach result.

Check every step so a failed stop/reset cannot fall through to launching or
probing an old service. For example, when GPU cleanup is explicitly intended for
the selected pod:

```bash
set -euo pipefail
okdev exec my-session --pod worker-0 --preflight-retry-timeout 30s --reset-gpu
launch=$(okdev exec my-session --pod worker-0 --preflight-retry-timeout 30s \
  --detach -- sh -c 'printf "STARTED\n"; exec python train.py')
printf '%s\n' "$launch"
# Keep the returned job ID; use jobs wait / jobs logs for that exact launch.
# STARTED alone does not prove service readiness or successful initialization.
```

Exit 74 means the session was absent; 78 means transient preflight contact failure.
After delivery starts, infrastructure failures use the exec delivery failure
path (69 for fanout), while remote exits remain command results. In `--json`
mode, produced envelopes are the result: inspect each `status`, `exit` and
`error`, even if okdev exits zero. `--require-all` fails for missing responses;
it does not turn every remote nonzero exit into a failing okdev process exit.
`--detach` does not support `--json`; a failed preflight prints no launch ID.

## Stop, start, and verify a new service instance

The service's `/ready` endpoint must return its inherited `OKDEV_JOB_ID` as the
whole response body, and only when initialization is complete. Returning a
constant `OK` is insufficient, and reading the expected ID from caller state
would bypass instance verification. For file-based probes, use a health check
plus an instance marker written by the launched service itself.

```bash
set -euo pipefail
: "${OLD_JOB_ID:?set the exact old detached job ID}"
okdev jobs stop "$OLD_JOB_ID" my-session --pod worker-0
launch=$(okdev exec my-session --pod worker-0 --detach -- \
  python /workspace/service.py)
job_id=$(printf '%s\n' "$launch" | sed -n 's/.*job_id=\([^ ]*\).*/\1/p' | head -n 1)
test -n "$job_id"
okdev jobs ready "$job_id" my-session --pod worker-0 \
  --timeout 2m --probe-timeout 5s \
  --probe 'curl -fsS --max-time 2 http://127.0.0.1:8000/ready'
okdev exec my-session --pod worker-0 -- python /workspace/client.py
```

Every stage is checked: a failed stop or launch prevents the next stage, and
readiness is tied to the returned job ID rather than an old healthy service.
Cleanup is restricted to that old job ID and pod; GPU reset is not a prerequisite.
For a job-specific log milestone, keep using `jobs wait <id> --grep PATTERN`.
Probe stdout is limited to 4096 bytes before surrounding whitespace is trimmed;
stderr is discarded. For a given attempt, overflow or a successful probe returning
the wrong ID takes precedence over a concurrent deadline in the rejection reason.
Later failed attempts can replace that reason. Partial output from a failed probe
is not treated as a completed identity; expired or canceled probes never establish
readiness.

For completion, keep using ordinary `jobs wait <id>`. `jobs ready` returns before
completion and fails if a tracked member exits, even successfully, before the
probe verifies readiness. See the command reference for member discovery,
deadlines and JSON output limits.
