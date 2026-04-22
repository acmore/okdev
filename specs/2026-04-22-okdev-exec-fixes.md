# okdev Exec Reliability Fixes

## Summary

This spec captures the `okdev exec` issues observed during the
`benchmark-jacky-he` GLM-5 benchmark session on 2026-04-22 and turns them
into a concrete engineering scope for `okdev`.

The source incident notes are in:

- `/Users/jacky.he/work/benchmark/docs/okdev-exec-issues-2026-04-22.md`

This document separates:

- confirmed product gaps in `okdev`
- likely implementation issues in current code
- unproven runtime hypotheses that still need live repro data

## Goals

- Make `okdev exec --detach` semantics predictable.
- Make `okdev exec` failure output actionable during live debugging.
- Improve resilience of non-interactive `okdev exec` calls to transient exec
  transport failures.
- Add first-class detached job visibility so users can inspect launch status
  without ad hoc `pgrep` and log scraping.

## Non-Goals

- Prove that every observed exit `137` was caused by `okdev`.
- Replace Kubernetes exec transport with SSH or another remote-exec mechanism.
- Redesign the entire `exec` CLI surface in one pass.
- Solve workload-local OOM or benchmark-client memory behavior inside user
  containers.

## Current State

### 1. `--detach` wraps commands in an extra background shell

Current detached execution builds:

```sh
sh -c "nohup sh -c '<user command>' >/dev/null 2>&1 &"
```

This means `okdev exec --detach` already backgrounds the command. If the user
also passes a shell snippet that backgrounds another process, process lifetime
becomes difficult to reason about.

Relevant code:

- `internal/cli/pdsh.go`: `detachCommand`
- `internal/cli/pdsh.go`: `runDetachExec`

### 2. Error reporting is low-signal

The multi-pod command path reports failures as:

```text
FAILED: <pod> (<error>)
```

and then returns a generic aggregate error. This is not enough for live
debugging because it does not clearly distinguish:

- remote process exit
- signal termination
- exec transport failure
- timeout
- command-start failure

Relevant code:

- `internal/cli/pdsh.go`: `runMultiExec`
- `internal/cli/root.go`: root Cobra command setup

### 3. Detached execution has no observability surface

After `okdev exec --detach`, the CLI reports success or failure of the launch
attempt, but it does not expose:

- remote PID
- log path
- command string
- start timestamp
- current status
- final exit code

Users currently need manual probes inside the container.

### 4. Non-interactive command exec is less resilient than interactive exec

The single-shell interactive path uses the retry wrapper in
`internal/connect/runner.go`.

The non-interactive command path used by `okdev exec -- <cmd>` resolves an
explicit target container and then calls `RunOnContainer`, which bypasses that
retry behavior when a container is set.

This likely makes command execution more fragile under transient SPDY exec
stream failures.

Relevant code:

- `internal/cli/common.go`: `runConnectWithClient`
- `internal/cli/pdsh.go`: `runMultiExec`
- `internal/connect/runner.go`: `Run`, `RunOnContainer`

### 5. Some incident repros omitted `--container serving`

The benchmark runbook for the same session consistently targets
`--container serving`, while `okdev` defaults to `dev` unless the config pins
another attach container.

This does not invalidate the reported issues, but future repro docs should name
the target container explicitly to avoid ambiguity.

## Problem Breakdown

### P0: Detached process semantics are misleading

Symptoms:

- `okdev` prints `detached`
- target process may not still be alive
- shell-wrapped commands behave differently from `exec <binary> ...`

Engineering problem:

- the current detach contract is "launch command through a shell and return",
  not "start a tracked detached job with defined lifetime semantics"

Required outcome:

- users must be able to predict what process becomes the detached job
- `okdev` must document and enforce one detach model

### P0: Exec failures are hard to triage quickly

Symptoms:

- generic `FAILED: ... exit code 137`
- usage/help output can dominate the actual failure

Engineering problem:

- failure classification is too coarse
- aggregate command errors lose useful per-pod and per-container context

Required outcome:

- error output must identify pod, container, failure kind, and exit/signal
  information when known

### P1: Command exec lacks transport retry parity

Symptoms:

- lightweight follow-up probes may fail during heavy workload periods
- behavior may be worse than interactive connect

Engineering problem:

- retry logic applies to some exec paths but not to explicit-container command
  execution

Required outcome:

- transient transport failures should be retried consistently across exec paths
  when retrying is safe

### P1: Detached jobs are opaque

Symptoms:

- no job id
- no status command
- no exit record

Engineering problem:

- `--detach` is treated as fire-and-forget instead of a manageable execution
  mode

Required outcome:

- detached execution must leave behind inspectable metadata

## Proposed Fixes

### Fix 1: Redefine detach around a single managed child process

Change `--detach` semantics so `okdev` launches one managed process per target
pod and records metadata for that process.

Rules:

- `okdev exec --detach -- <cmd>` should treat `<cmd>` as the detached command.
- `okdev` should not require users to add `nohup`, `&`, or extra shell
  wrapping.
- Documentation should explicitly say that users should not shell-background
  commands inside `--detach`.

Implementation direction:

- keep a shell wrapper only when needed to preserve current argument handling
- inside the remote wrapper, start exactly one background child
- capture its PID
- write structured job metadata to a stable path in the target container
- return only after metadata is written successfully

Open choice:

- minimal version: keep shell-based detach but record PID and status files
- stronger version: stage a small remote wrapper script and use `exec` to reduce
  shell lifetime ambiguity

Recommendation:

- start with the minimal version because it is narrower and fits the current
  architecture

### Fix 2: Add detached job metadata and a status command

Add first-class detached job inspection.

Minimum UX:

- `okdev exec --detach ...` prints:
  - pod
  - container
  - job id
  - pid
  - log path
- add a follow-up command, preferably:
  - `okdev exec-jobs`
  - or `okdev exec --jobs`

Job metadata should include:

- job id
- pod
- container
- pid
- full command string
- launch time
- stdout path
- stderr path
- current state: `running|exited|unknown`
- exit code if known

Storage model:

- store metadata under a dedicated okdev-managed directory in the target
  container, for example `/tmp/okdev-exec` or `/var/okdev/exec`
- keep format simple JSON for easy remote inspection and future extension

### Fix 3: Improve failure classification and CLI output

Improve per-pod failure reporting in `runMultiExec`.

Desired error categories:

- remote non-zero exit
- remote signal termination
- timeout
- transport error
- container-selection error
- command-start/setup error

Desired output example:

```text
FAILED: pod=okdev-benchmark-jacky-he-ac792b41-0 container=serving kind=remote-exit exit=137 signal=KILL
```

or:

```text
FAILED: pod=okdev-benchmark-jacky-he-ac792b41-0 container=serving kind=transport error="stream error: ..."
```

Root command behavior:

- suppress automatic usage/help output for ordinary runtime command failures
- keep usage output for actual invocation/flag validation errors

### Fix 4: Restore retry parity for safe non-interactive exec paths

Refactor `internal/connect/runner.go` so container-targeted exec can use the
same retry policy as non-container exec.

Possible shape:

- add a container-aware retry wrapper instead of bypassing retries in
  `RunOnContainer`
- retry only for known transport-level transient errors
- never retry when the remote process definitely started and returned a real
  exit code

This requires preserving the distinction between:

- remote command failure
- transport failure before or during stream setup

### Fix 5: Tighten docs and repro guidance

Update `okdev` command docs to state:

- `--detach` already backgrounds the command
- do not add `nohup ... &` inside detached shell snippets
- prefer explicit `--container` in multi-container workloads
- explain where detached logs and metadata live

## Acceptance Criteria

### Detach behavior

- `okdev exec --detach -- python long_running.py` starts a durable remote job
  and returns a job id, pid, and log path.
- The CLI does not report `detached` unless job metadata was written
  successfully.
- A detached job remains inspectable after the launching exec stream ends.

### Status visibility

- Users can query detached jobs without ad hoc shell commands.
- Status output shows whether the process is still alive and, if finished, its
  exit code.

### Error output

- Failed non-interactive exec output always includes pod and container.
- Runtime failures do not dump the full command usage block.
- Transport errors are distinguishable from remote process exits.

### Retry behavior

- Container-targeted non-interactive exec retries on transient transport
  failures.
- Remote command exits are not retried.

## Testing Strategy

### Unit tests

- `detachCommand` or its replacement builds the expected remote wrapper.
- detach launch logic records pid and metadata paths.
- failure summaries include pod, container, and classified failure kind.
- root command does not emit usage text for runtime exec failures.
- container-targeted exec path uses retry policy for retryable transport
  failures.
- remote non-zero exit is not retried.

### Integration tests

- fake exec client returns transient transport error once, then success:
  command path should succeed after retry
- fake exec client returns remote exit error:
  command path should fail without retry
- detached launch writes metadata and status can read it back

### Live validation

Use a real multi-container session and verify:

- explicit `--container serving` command execution
- detached `python -c 'import time; time.sleep(60)'`
- detached shell snippet without internal `&`
- failed command with known exit code
- simulated transport disruption if a safe repro is available

## Out of Scope Questions That Still Need Live Evidence

These remain open after code review:

- whether the reported exit `137` cases were remote process OOMs in the target
  container
- whether any failures came from Kubernetes exec transport instability under
  heavy load
- whether sidecar or pod resource pressure contributed to stream drops

These should be investigated with improved error classification rather than
guessed in advance.

## Recommended Implementation Order

1. Improve runtime error rendering and suppress noisy usage output.
2. Restore retry parity for container-targeted non-interactive exec.
3. Implement minimal detached job metadata and surfaced launch details.
4. Add detached job status inspection.
5. Update docs and add live-validation notes.
