# okdev

Lightweight Kubernetes-native dev environments for AI infra engineering.

- PodSpec-first config via `.okdev.yaml`
- Multi-session support per repo/branch/user
- Shell/SSH access, sync, and port forwarding
- Cross-machine reattach using cluster-native session identity
- Syncthing-based sync engine (`okdev sync`)

## Install

Latest release:

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
```

Specific version:

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh -s -- v0.1.0
```

Binary assets are published to GitHub Releases for `linux/darwin` on `amd64/arm64`.

See:
- `docs/quickstart.md`
- `docs/command-reference.md`
- `docs/config-manifest.md`
- `docs/troubleshooting.md`
- `docs/release.md`

## Development

Coverage on some Go 1.25 toolchains can fail under `go test ./... -cover` because the bundled `covdata` tool is missing for packages without tests. Use the repo helper instead:

```bash
./scripts/coverage.sh
```

Formatting is enforced through `pre-commit` and CI. To enable the local pre-push check:

```bash
python3 -m pip install --user pre-commit
pre-commit install --hook-type pre-push
```

You can run the same formatter check on demand with:

```bash
pre-commit run --all-files --hook-stage manual okdev-gofmt
```

## GitHub Pages Docs

Docs are published from `docs/` via GitHub Actions + MkDocs.

- Workflow: `.github/workflows/docs.yml`
- Source config: `mkdocs.yml`
- Expected URL: `https://acmore.github.io/okdev/`

## License

Apache License 2.0. See `LICENSE`.
