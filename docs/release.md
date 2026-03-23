# Release & Versioning

## Versioning

`okdev` uses Semantic Versioning:

- `MAJOR`: breaking CLI/config behavior changes.
- `MINOR`: backward-compatible features.
- `PATCH`: backward-compatible fixes.

Tag format:

- `vX.Y.Z` (example: `v0.2.1`)

## Release Workflow

1. Merge to `main`.
2. Create and push a release tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

3. GitHub Actions `Release` workflow builds binaries for:
   - `linux/amd64`
   - `linux/arm64`
   - `darwin/amd64`
   - `darwin/arm64`
4. Workflow publishes assets:
   - `okdev_<os>_<arch>.tar.gz`
   - `checksums.txt`

## Sidecar image tags

Sidecar image tags are aligned with the `okdev` release tag:

- release `vX.Y.Z` publishes:
  - `ghcr.io/acmore/okdev:vX.Y.Z` (sidecar runtime: syncthing + okdev-sshd binary)
- dev/main pushes publish:
  - `ghcr.io/acmore/okdev:edge`

Default sidecar image resolution uses the running `okdev` binary version, with `edge` fallback for dev builds.

## Install From Release

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
```

Install specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh -s -- v0.1.0
```
