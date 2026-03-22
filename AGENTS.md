# Agent Guidelines

This file is the canonical repository guidance for coding agents working in `okdev`.

## Workflow

- Prefer targeted, repo-relevant tests before committing.
- Prefer `rg` and `rg --files` for search.
- Keep changes narrow and preserve existing patterns unless the task requires a broader refactor.
- Update user-facing docs when behavior changes.
- Do not retag old releases to fix issues; cut a new version instead.

## Editing Rules

- Never revert unrelated user changes.
- Avoid destructive git operations unless explicitly requested.
- Prefer small, reviewable commits with clear messages.
- Keep comments sparse and only where they clarify non-obvious logic.

## CLI UX Rules

- Interactive transient status output must clear cleanly and must not pollute table or machine-readable output.
- `okdev ssh-proxy` must stay silent by default; diagnostics should only appear behind explicit debug or verbose behavior.
- `okdev ssh` and tmux-backed sessions must land in the dev container, not the sidecar.
- Tmux behavior should preserve the normal shell experience and avoid exposing wrapper implementation details unless intentionally designed.

## Build And Release Rules

- `edge` images must always be built and pushed explicitly as `linux/amd64`.
- Do not rely on the local Docker default platform when publishing `edge`.
- When asked to update a local install to a release, prefer the published release asset over a workspace build.
- For release follow-ups, verify the actual published artifact or image rather than assuming the workflow succeeded.

## Repo-Specific Expectations

- Keep sidecar, SSH, tmux, and Syncthing behavior aligned; changes in one often require checking the others.
- Be careful with config-loading output paths because many commands share the same helper logic.
- When changing pod or sidecar behavior, consider both local installs and sidecar image rebuild/publish steps.

