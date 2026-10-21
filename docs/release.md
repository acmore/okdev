# Release & Versioning

## Versioning

`okdev` uses Semantic Versioning:

- `MAJOR`: breaking CLI/config behavior changes.
- `MINOR`: backward-compatible features.
- `PATCH`: backward-compatible fixes.

### Tag Format

- `vX.Y.Z` (example: `v0.2.1`)
- prerelease/testing: `vX.Y.Z-rcN` (example: `v0.3.0-rc1`)

## Release Workflow

1. Merge to `main`.
2. Create and push a release tag:

```bash
git tag v0.1.0
git push origin v0.1.0
```

3. GitHub Actions `Release` workflow first requires successful `CI` and `E2E Kind` runs for the tagged commit (both run on every push to `main`), then builds binaries for `linux/amd64`, `linux/arm64`, `darwin/amd64`, and `darwin/arm64`. If GitHub dropped the push event so a `main` commit has no runs, the hourly `Main Checks Sweep` workflow dispatches the missing ones; you can also start `CI` or `E2E Kind` manually on that ref (workflow_dispatch) and re-run the release.
4. Workflow publishes `okdev_<os>_<arch>.tar.gz` assets plus `checksums.txt`.
5. Workflow signs a GitHub build provenance attestation for every archive (Sigstore keyless, `actions/attest-build-provenance`). `okdev upgrade` refuses archives without one; anyone can check an archive with:

```bash
gh attestation verify okdev_linux_amd64.tar.gz --repo acmore/okdev \
  --signer-workflow acmore/okdev/.github/workflows/release.yml
```

### Prereleases

- Tags with a suffix like `-rc1` are published as GitHub prereleases.
- Prereleases are opt-in for installers:

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh -s -- v0.3.0-rc1
```

- The default install path still resolves the latest stable release.

## Sidecar image tags

### Published Tags

- release `vX.Y.Z` publishes:
  `ghcr.io/acmore/okdev:vX.Y.Z` (sidecar runtime: syncthing + okdev-sshd binary)
- prerelease `vX.Y.Z-rcN` publishes:
  `ghcr.io/acmore/okdev:vX.Y.Z-rcN`
- dev/main pushes publish:
  `ghcr.io/acmore/okdev:edge`
- `latest` is only published for stable `vX.Y.Z` tags, not prereleases.

Default sidecar image resolution uses the running `okdev` binary version, with `edge` fallback for dev builds.

The sidecar image build verifies Syncthing's clearsigned `sha256sum.txt.asc` against Syncthing's release keys pinned in `internal/syncthing/release-keys.asc` (the same keys the CLI uses when it auto-installs Syncthing locally) before trusting any checksum.

## Install From Release

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
```

The installer requires the GitHub CLI (`gh`) to be installed and logged in. It verifies the archive against `checksums.txt` and verifies a build provenance attestation from this repository's release workflow for the selected tag. For an older release without an attestation, set `OKDEV_ALLOW_UNATTESTED=1` to explicitly install on the checksum alone; this also allows installation without `gh`. An attestation that exists but fails verification is never accepted.

### Install Specific Version

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | OKDEV_ALLOW_UNATTESTED=1 sh -s -- v0.1.0
```
