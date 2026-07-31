# okdev

**Kubernetes-native remote dev environments for AI/ML engineers.**
Edit code on your laptop, run it on cluster GPUs — with live file sync, a real shell in the pod, and first-class support for multi-pod distributed training.

[![Release](https://img.shields.io/github/v/release/acmore/okdev?sort=semver)](https://github.com/acmore/okdev/releases)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue)](LICENSE)
[![Docs](https://img.shields.io/badge/docs-acmore.github.io%2Fokdev-informational)](https://acmore.github.io/okdev/)

---

## The problem

Your GPUs are in a cluster. Your editor, your git history, and your muscle memory are not.

The usual workarounds all leak: `kubectl cp` before every run, a Docker rebuild to change one line, a Jupyter tab standing in for a terminal, or an SSH bastion that forgets everything the moment the pod restarts.

`okdev` makes the cluster feel like localhost. One config file, one `okdev up`, and your working tree is live inside a pod you can SSH into.

```console
$ okdev up

== Ready ==
session:   myproj-alice
namespace: ml-team
pod:       okdev-myproj-alice-3bce18ff
ssh:       ssh okdev-myproj-alice
sync:      active (<->)
sync paths:
- /Users/alice/work/myproj <-> /workspace
forwards:
- app: localhost:8080 -> remote:8080
```

```console
$ vim train.py                                 # edit locally, in your editor
$ okdev exec --require-sync -- python train.py # runs on the cluster, never on stale code
```

---

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/acmore/okdev/main/scripts/install.sh | sh
```

Prebuilt binaries for `linux` and `darwin` on `amd64` and `arm64`. Already installed? `okdev upgrade`.

## 60-second start

```bash
cd your-repo
okdev init                # writes .okdev/okdev.yaml
okdev up                  # creates the pod, starts sync, forwards ports
okdev ssh                 # you are in the cluster, in your repo
okdev down                # tear it down
```

---

## What you get

### Your code, actually live

Bidirectional [Syncthing](https://syncthing.net/)-backed sync between your working tree and the pod — not a one-shot copy. `okdev sync wait` blocks until every byte has landed, and `okdev exec --require-sync` *refuses to launch* until it has, so a run can never silently use pre-edit code.

Sync is self-healing (it repairs broken peer connections on its own), pausable (`okdev sync pause` before a `git checkout`, so a branch switch cannot propagate onto a running job), and honest about failure — a dead channel is reported as dead rather than as "no pending changes".

### A real shell, not a notebook

`okdev ssh` drops you into the dev container, optionally inside a tmux-backed persistent session so a closed laptop lid does not kill a training run. Port forwards come up with the session. Your editor's remote-SSH mode works, because it is just SSH.

### Multi-pod distributed training, first-class

Run a `Pod`, a `Job`, a **`PyTorchJob`**, or any manifest you already have (`Deployment`, or anything else — `generic`). Declare several and switch between them without touching the rest of your setup:

```yaml
spec:
  workloads:
    - name: dev                     # single pod for iteration
      type: pod
    - name: train                   # multi-GPU distributed run
      type: pytorchjob
      manifestPath: train.yaml
      inject: [{path: spec.pytorchReplicaSpecs.Worker.template}]
```

```bash
okdev workload use train && okdev up   # same session, different shape
```

okdev injects its sidecar into every replica, wires up inter-pod SSH, and gives every pod a stable short name (`master-0`, `worker-3`) that `--pod`, the in-pod `/etc/hosts`, and `MASTER_ADDR` all agree on.

### Fleet operations that scale past `kubectl exec`

```bash
okdev exec --fanout 16 -- nvidia-smi          # every pod, one command
okdev exec --detach -- python train.py        # survives your terminal
okdev jobs logs <id> --tail 100 --grep loss   # follow it later
okdev exec --reset-gpu                        # kill GPU holders, verify clear
```

At high pod counts, fanout routes through a single gateway pod over the pod network instead of N proxied apiserver streams — the difference between "works at 4 pods" and "works at 64".

### Setup that survives a recreate

`spec.lifecycle.postCreate` and `postSync` re-run automatically on every recreated pod, so installed tools and builds come back without you remembering. `okdev env-diff` lists what you changed by hand while debugging and drafts the hook for you.

### Coding agents

Configure `claude-code`, `codex`, `gemini`, or `opencode` under `spec.agents`; okdev installs them and stages your local auth into the session container.

---

## Docs

| | |
|---|---|
| [Quickstart](https://acmore.github.io/okdev/quickstart/) | From zero to a running session |
| [Config manifest](https://acmore.github.io/okdev/config-manifest/) | Every field, with examples |
| [Command reference](https://acmore.github.io/okdev/command-reference/) | Every command and flag |
| [Troubleshooting](https://acmore.github.io/okdev/troubleshooting/) | When something is off |

---

## Contributing

Requirements: Go 1.21+, `kubectl` pointed at a cluster, and namespace permissions to create Pods and PVCs.

```bash
go build -o bin/okdev ./cmd/okdev
```

Formatting is enforced by `pre-commit` and CI:

```bash
uv venv .venv
uv pip install --python .venv/bin/python pre-commit
.venv/bin/pre-commit install --hook-type pre-commit --hook-type pre-push
```

Run the same check on demand with
`.venv/bin/pre-commit run --all-files --hook-stage manual okdev-gofmt`.

Coverage on some Go 1.25 toolchains fails under `go test ./... -cover` because the
bundled `covdata` tool is missing for packages without tests — use
`./scripts/coverage.sh` instead.

For end-to-end checks against a reusable Kind cluster:

```bash
bash scripts/e2e_local_kind.sh
```

That covers the template, smoke, deployment, job, multi-session, and
workload-switching scenarios. Sync- and reconcile-heavy changes can add
`RUN_PYTORCHJOB=1`, or a local-only large-repo stress check:

```bash
RUN_LARGE_REPO=1 LARGE_REPO_PATH=~/workspace/pytorch bash scripts/e2e_local_kind.sh
RUN_LARGE_REPO=1 LARGE_REPO_URL=https://github.com/pytorch/pytorch.git bash scripts/e2e_local_kind.sh
```

Never judge an e2e run through a pipe — `| tail` reports `tail`'s exit status, not
the suite's. Redirect the run and capture the real one. See `AGENTS.md` for the
full set of repository conventions.

## License

Apache License 2.0. See [LICENSE](LICENSE).
