# okdev

**CRD-less Kubernetes dev environments from a single manifest.**

okdev provisions and operates dev sessions using standard Pod/PVC resources -- no operators, no CRDs, no cluster-side components.

---

## How it works

```
.okdev.yaml  -->  okdev up  -->  Pod + PVC + SSH + Sync
```

1. Define your environment in `.okdev.yaml` (image, volumes, ports, sync paths).
2. Run `okdev up` to create the pod, configure SSH, and start file sync.
3. Connect with `ssh okdev-<session>` or `okdev connect`.

---

## Key features

| | |
|---|---|
| **One-command setup** | `okdev up` handles pod creation, SSH config, port forwarding, and sync |
| **Persistent sessions** | tmux-backed shells survive disconnects by default |
| **File sync** | Bidirectional Syncthing sync with exclude patterns |
| **Port forwarding** | Declare ports in config, get automatic SSH `LocalForward` entries |
| **Multi-session** | Run multiple environments per repo with named sessions |
| **No cluster deps** | Works with any Kubernetes cluster -- RBAC for Pods and PVCs is all you need |

---

## Quick start

```bash
# Install
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh

# Initialize config
okdev init

# Start session
okdev up

# Connect
ssh okdev-<session>
```

See the [Quickstart guide](quickstart.md) for details.

---

## Documentation

- [Quickstart](quickstart.md) -- install, configure, and launch your first session
- [Config Manifest](config-manifest.md) -- full `.okdev.yaml` field reference
- [Command Reference](command-reference.md) -- every CLI command and flag
- [Troubleshooting](troubleshooting.md) -- common issues and fixes
- [Release & Versioning](release.md) -- release process and image tags
