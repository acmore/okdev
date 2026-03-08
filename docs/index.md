# okdev

`okdev` is a CRD-less CLI for Kubernetes development sessions.
It provisions and operates dev environments from `.okdev.yaml` using standard Pod/PVC resources.

## Core Capabilities

- PodSpec-driven environment definition
- Multi-session workflow per repo/branch/user
- Session setup via `okdev up` (SSH config, managed forwards, sync bootstrap)
- Syncthing-based workspace synchronization
- Reattach from any machine with kube access

## Documentation

- [Quickstart](quickstart.md)
- [Command Reference](command-reference.md)
- [Troubleshooting](troubleshooting.md)
- [Design](okdev-design.md)
- [Implementation Plan](okdev-implementation-plan.md)
- [Release & Versioning](release.md)
