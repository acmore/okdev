# Release & Versioning

## Versioning

`okdev` uses Semantic Versioning:

- `MAJOR`: breaking CLI/config behavior changes.
- `MINOR`: backward-compatible features.
- `PATCH`: backward-compatible fixes.

### Tag Format

- `vX.Y.Z` (example: `v0.2.1`)

## Release Workflow

1. Merge to `main`.
2. Create and push a release tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

3. GitHub Actions `Release` workflow builds binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`.
4. Workflow publishes `okdev_<os>_<arch>.tar.gz` assets plus `checksums.txt`.

## Sidecar image tags

### Published Tags

- release `vX.Y.Z` publishes:
  `ghcr.io/acmore/okdev:vX.Y.Z` (sidecar runtime: syncthing + okdev-sshd binary)
- dev/main pushes publish:
  `ghcr.io/acmore/okdev:edge`

Default sidecar image resolution uses the running `okdev` binary version, with `edge` fallback for dev builds.

## Install From Release

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
```

### Install Specific Version

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh -s -- v0.1.0
```
