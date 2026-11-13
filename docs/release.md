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

### Minimum supported CLI version

The repository-wide floor is configured in `.github/release-policy.json` (`schemaVersion`, `minimumCliVersion`, `enforceAfter`).

Change this file through a reviewed PR, then publish a new release. The release workflow validates and attests the policy as a separate release asset. Keep `minimumCliVersion` at or below the release tag; raise it only after a release containing the policy checker has been distributed. Existing binaries that predate the checker cannot be made to enforce the policy retroactively.

Released CLIs verify the policy's Sigstore attestation against this repository's release workflow and exact release tag before using it, and ask GitHub for it at most once every 24 hours, so a raised floor can take up to a day to reach a client. Set `enforceAfter` with that in mind.

The floor is an upgrade signal, not a security boundary, so it fails open: only a verified policy that puts the running version below the floor blocks commands (all except `version`, `upgrade`, help and shell completion). If the policy cannot be fetched or verified, the last verified policy keeps applying; a client that never verified one runs normally. Failures are recorded in `~/.okdev/logs/okdev.log` and never printed, so `okdev ssh-proxy` stays silent unless it is blocked. Development builds do not check the release policy.

The floor a client enforces only increases: a later policy with a lower `minimumCliVersion` is ignored, so lowering the floor in the repository does not unblock clients that already saw the higher one. Withdrawing a release is safe, because the release tag itself may go backwards. To drop a floor raised by mistake, affected users delete `~/.okdev/upgrade/release_policy.json`.

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
