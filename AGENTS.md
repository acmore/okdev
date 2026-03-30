# Agent Guidelines

This file is the canonical repository guidance for coding agents working in `okdev`.

## Workflow

- Prefer targeted, repo-relevant tests before committing.
- Run the formatting check before commit and push:
  `.venv/bin/pre-commit run --all-files --hook-stage manual okdev-gofmt`
- Local git hooks should be installed from the repo venv and enforce both `pre-commit` and `pre-push`:
  `uv venv .venv && uv pip install --python .venv/bin/python pre-commit && .venv/bin/pre-commit install --hook-type pre-commit --hook-type pre-push`
- Clean up resources created by ad-hoc e2e tests before finishing the task.
- Prefer `rg` and `rg --files` for search.
- Keep changes narrow and preserve existing patterns unless the task requires a broader refactor.
- Update user-facing docs when behavior changes.
- Do not retag old releases to fix issues; cut a new version instead.
- Prefer opening a PR rather than pushing direct branch history for review workflows.
- Prefer squash merge when merging PRs unless the user explicitly wants to preserve individual commits.

## Editing Rules

- Never revert unrelated user changes.
- Avoid destructive git operations unless explicitly requested.
- Prefer small, reviewable commits with clear messages.
- Keep comments sparse and only where they clarify non-obvious logic.

## CLI UX Rules

- Interactive transient status output must clear cleanly and must not pollute table or machine-readable output.
- `okdev ssh-proxy` must stay silent by default; diagnostics should only appear behind explicit debug or verbose behavior.
- `okdev ssh` and tmux-backed sessions run inside the dev container via `okdev-sshd`.
- Tmux behavior should preserve the normal shell experience and avoid exposing wrapper implementation details unless intentionally designed.

## Build And Release Rules

- `edge` images must always be built and pushed explicitly as `linux/amd64`.
- If the user asks to rebuild `edge`, treat that as build-and-push by default unless they explicitly say local-only.
- Do not stop at a successful local `edge` rebuild; push the image in the same task unless the user explicitly asked for a local-only build.
- If the user asks to rebuild the CLI binary and a local install path exists, update the local install in the same task unless the user explicitly asked for a repo-local build only.
- Do not rely on the local Docker default platform when publishing `edge`.
- When asked to update a local install to a release, prefer the published release asset over a workspace build.
- For release follow-ups, verify the actual published artifact or image rather than assuming the workflow succeeded.

## E2E Testing Rules

- Prefer the smallest realistic end-to-end check that validates the changed behavior.
- Reuse existing local installs, release binaries, and published sidecar images when they are sufficient for the test.
- If an e2e test creates cluster resources, local background processes, SSH config entries, port-forwards, or temporary files, clean them up before finishing unless the user explicitly asks to keep them.
- For `okdev` session tests, prefer tearing down with `okdev down` after verification when the session was created only for the test.
- If cleanup cannot be completed, say exactly what was left behind and where.

## Repo-Specific Expectations

- Keep sidecar (syncthing), SSH (`okdev-sshd`), tmux, and Syncthing behavior aligned; changes in one often require checking the others.
- Be careful with config-loading output paths because many commands share the same helper logic.
- When changing pod or sidecar behavior, consider both local installs and sidecar image rebuild/publish steps.
