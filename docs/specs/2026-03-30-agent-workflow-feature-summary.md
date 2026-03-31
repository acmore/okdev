# Agent Workflow Feature Summary

This note summarizes the ideas discussed for improving `okdev` around coding-agent workflows, unattended execution, debugging, and sandboxing.

## Goals

- Make AI-assisted workflows faster and less fragile.
- Improve unattended execution safely.
- Increase observability into agent behavior.
- Strengthen isolation so higher autonomy is safer.
- Add targeted features that help developers debug agent/session issues.

## High-Value Product Features

### `okdev doctor`

A session-aware diagnostic command that checks:

- target pod and container state
- sidecar readiness
- sync health
- SSH service readiness
- tmux availability
- workspace mounts
- common misconfigurations

Why it matters:

- reduces time spent gathering context
- gives agents and users a consistent starting point for debugging

### `okdev exec`

A first-class non-interactive command runner instead of overloading `okdev ssh --cmd`.

Desired properties:

- structured exit status
- stable output behavior
- JSON mode
- predictable container/target selection

Why it matters:

- agents do a lot of non-interactive work
- cleaner automation surface than interactive SSH

### `okdev snapshot`

Capture a session support bundle with:

- config path and resolved config
- target pod/container
- annotations and labels
- pod/container status
- recent events
- sync state
- SSH/tmux state

Why it matters:

- simplifies issue reports
- useful for unattended runs and postmortems

### `okdev diff`

Show effective workload/config drift before `up`.

Possible scopes:

- config drift
- rendered workload drift
- local vs applied workload snapshot

Why it matters:

- helps users and agents understand what changed before applying

### `okdev trace`

Verbose timeline-style tracing for operations like:

- `up`
- `ssh`
- `sync`
- `down`

Should include:

- major steps
- timings
- retries
- failure points

Why it matters:

- useful when behavior is flaky or slow

### `okdev apply --print`

Print the fully rendered workload after injection/mutation.

Why it matters:

- agents often need to inspect the effective manifest, not just source config

### `okdev runbook`

Guided remediation for known failure classes:

- image pull failures
- SSH not ready
- sync stuck
- tmux unavailable
- target pod drift

Why it matters:

- turns repeated troubleshooting into a productized flow

## Logging And Debugging Ideas

### PTY/session transcript logging

Discussion outcome:

- useful for diagnostics
- not suitable as a default behavior
- should be opt-in

Possible shapes:

- client-side transcript: `okdev ssh --record`
- server-side transcript written to a dedicated file
- separate viewing command, such as `okdev ssh-log`

Why it should not be default:

- noisy
- contains terminal escape sequences
- may capture secrets and sensitive output

### Better `okdev logs`

Problem observed:

- default logs are not very useful when the dev container just runs `sleep`

Idea discussed:

- stream more useful sources by default in common cases
- expose agent-focused combined logs

Possible follow-up:

- `okdev logs --agent`
- include sidecar, SSHD, lifecycle, and other relevant diagnostic sources

## Skills And Agent Capability Ideas

### Optional installable skills

Idea:

- keep the default agent behavior generic
- offer optional skills users can enable when needed

Example skill categories:

- Kubernetes debugging
- frontend design
- Python/Node repo helpers
- release automation
- data/SQL investigation

Why optional is better:

- reduces prompt noise
- keeps behavior predictable
- allows per-task specialization

Possible UX:

- `okdev agent skills list`
- `okdev agent skills install <name>`
- `okdev agent run --skill <name>`

### Features that improve users' AI workflow skill

Not purely product features, but workflow multipliers:

- prompt/task framing templates
- explicit work modes: brainstorm, implement, debug, review, release
- missing-context hints
- better post-task verification discipline
- checklists and runbooks
- more machine-readable tooling

Why it matters:

- the best gains often come from better user workflow, not more model power

## Unattended Workflow Permission Model

### Higher-permission unattended workflows

Discussion outcome:

- useful and likely necessary for strong agent autonomy
- should not mean full unrestricted trust

Recommended model:

- capability grants instead of blanket trust
- allow safe command classes without prompting every time
- keep destructive or high-risk actions gated

Good unattended buckets:

- build/test
- non-destructive git
- cluster read access
- local installs
- release inspection

High-risk buckets that should stay gated:

- destructive git operations
- cluster deletes/applies outside managed scope
- package/release deletion
- writes outside approved roots

### Permission presets

Question discussed:

- can Codex support named permission presets like `safe`, `devops`, `maintainer`?

Conclusion:

- not as a strong native built-in today
- partially approximated via approved command-prefix rules
- best implemented in the host/runtime layer rather than the model layer

Recommended preset examples:

- `safe`
- `devops`
- `maintainer`
- `dangerous`

Each preset should define:

- allowed command prefixes
- optional path/network scopes
- explicit denies for destructive operations

### Recommended `okdev` permission preset shape

The cleanest product model is per-agent policy, not one global switch.

Suggested config shape:

```yaml
spec:
  agents:
    - name: codex
      policy:
        preset: dev
    - name: claude-code
      policy:
        preset: safe
```

Allow small, explicit overrides on top of a built-in preset:

```yaml
spec:
  agents:
    - name: codex
      policy:
        preset: dev
        allowCommands:
          - ["go", "test"]
          - ["go", "build"]
        denyCommands:
          - ["git", "push", "--force"]
        writablePaths:
          - "."
        readonlyPaths:
          - "~/.kube/config"
```

Recommended built-in presets:

- `safe`
  - read-only repo inspection
  - non-destructive build/test
  - no git mutation
  - no cluster mutation
- `dev`
  - `safe` plus repo writes, `git add`, and `git commit`
- `devops`
  - `dev` plus `kubectl get/logs/describe`, `docker build/push`, and `okdev up/down/ssh`
- `maintainer`
  - `devops` plus tag, release, and PR operations
- `dangerous`
  - explicit catch-all for fully trusted sessions only

Important design rules:

- match exact argv prefixes, not raw shell text
- deny rules always win over allow rules
- path scopes are first-class, not optional
- cluster/network scopes should stay narrow even in higher presets
- every auto-approved action should log the active preset and matched rule

Recommended product surface:

- config schema in `spec.agents[].policy`
- `okdev agent list` shows the active preset and override summary
- `okdev agent policy render` outputs a machine-readable policy for the host/runtime
- future unattended agent runners consume the rendered policy instead of re-encoding rules independently

Important boundary:

- `okdev` should define and render policy
- the actual auto-approval enforcement still belongs in the host or runner layer, not in the model itself

### Forking Codex to add native permission presets

Discussion outcome:

- reasonable engineering direction if you control the host layer
- permission presets belong in the runner/policy/UI, not the model itself

Recommended pieces:

- named preset definitions
- deterministic command-prefix matching
- session-level preset selection
- audit log of auto-approved actions
- explicit escalation for operations outside the preset

## Sandbox And Isolation

### Stronger sandboxing inside containers

Discussion outcome:

- one of the best enablers for safe autonomous workflows

Why it matters:

- lets users grant more autonomy with less risk
- contains mistakes
- protects host files and credentials

Recommended directions:

- isolated container or VM execution
- minimal mounts
- no implicit access to host `~`, SSH keys, kubeconfig, cloud creds
- brokered access for sensitive capabilities
- network restrictions
- resource limits
- ephemeral per-task environments

### Secure `okdev` agent sandbox shape

Possible model:

- dedicated tools container for the agent
- repo mounted read-write
- selective caches mounted
- credentials mediated through narrow capability grants
- explicit cluster/release/git capabilities
- no broad host-home access

## Activity Tracking For Unattended Agents

### What should be tracked

Discussion outcome:

- activity tracking should be an audit trail, not just plain logs

Recommended layers:

1. Intent

- requested task
- plan
- assumptions
- permission preset in effect

2. Actions

- commands run
- files read/edited
- cluster operations
- networked external operations

3. Outputs

- diffs
- test/build results
- artifacts
- PRs/releases/resources created

4. Outcome

- success/failure
- cleanup status
- residual risks

### Implementation shape

Recommended:

- structured event logs
- runtime-captured command audit
- file-change summaries
- external action audit
- human-readable timeline
- artifact bundle per run

Important note:

- do not rely only on model-generated summaries
- source of truth should come from the runtime/tool layer

## Operational Observations From The Session

These were concrete issues found during live debugging:

### Tmux client buildup on abrupt disconnect

Observed:

- abrupt local disconnect left stale tmux clients in the pod
- reconnects accumulated more tmux clients

Fix implemented separately:

- PTY cleanup in `okdev-sshd` now closes earlier when session input ends

### Ghostty TERM handling mismatch

Observed:

- local terminal type was not propagated correctly
- server-side normalization was only applied in the tmux bootstrap path

Fix implemented separately:

- client now requests the local terminal type
- server normalizes Ghostty for all interactive shells

## Suggested Priorities

If prioritizing for impact, the strongest next steps are:

1. `okdev doctor`
2. `okdev exec`
3. unattended permission presets in the runtime layer
4. structured unattended activity audit trail
5. stronger isolated agent sandbox
6. `okdev snapshot`
7. `okdev diff`
8. opt-in PTY transcript recording

## Overall Conclusion

The strongest theme from the discussion is:

- more autonomy is useful
- but it must be paired with stronger isolation, better observability, and clearer capability boundaries

The best investments are not only new agent commands, but also:

- secure runtime design
- explicit permission models
- better diagnostic surfaces
- structured auditability

These are what make unattended AI workflows practical and trustworthy.
