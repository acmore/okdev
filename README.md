# okdev

Lightweight Kubernetes-native dev environments for AI infra engineering.

- PodSpec-first config via `.okdev.yaml`
- Multi-session support per repo/branch/user
- Shell/SSH access, sync, and port forwarding
- Cross-machine reattach using cluster-native session identity
- Optional Syncthing sync engine (`okdev sync --engine syncthing`)

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
