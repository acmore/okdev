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
- `docs/okdev-design.md`
- `docs/okdev-implementation-plan.md`

## GitHub Pages Docs

Docs are published from `docs/` via GitHub Actions + MkDocs.

- Workflow: `.github/workflows/docs.yml`
- Source config: `mkdocs.yml`
- Expected URL: `https://acmore.github.io/okdev/`

## License

Apache License 2.0. See `LICENSE`.
