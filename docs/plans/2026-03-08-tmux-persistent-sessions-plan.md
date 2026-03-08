# Tmux Persistent Shell Sessions Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Wrap interactive SSH sessions in tmux on the server side so shells survive network drops, controlled by config and CLI flags.

**Architecture:** The sidecar's `nsenter-dev.sh` ForceCommand script conditionally wraps interactive shells in tmux based on the `OKDEV_TMUX` env var set on the pod. Config field `spec.ssh.persistentSession` and CLI flag `--tmux` control the env var. Per-connection opt-out via `--no-tmux` on `okdev ssh`.

**Tech Stack:** Go (config/CLI), shell script (sidecar entrypoint), Alpine apk (tmux package)

---

### Task 1: Add `PersistentSession` to config

**Files:**
- Modify: `internal/config/config.go:95-102` (SSHSpec struct)
- Modify: `internal/config/config.go:104-143` (SetDefaults)
- Test: `internal/config/config_test.go`

**Step 1: Write the failing test**

Add to `internal/config/config_test.go`:

```go
func TestSetDefaultsPersistentSessionNil(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	if cfg.Spec.SSH.PersistentSession != nil {
		t.Fatal("expected persistentSession to remain nil (off by default)")
	}
}

func TestSetDefaultsPersistentSessionExplicit(t *testing.T) {
	cfg := validConfig()
	v := true
	cfg.Spec.SSH.PersistentSession = &v
	cfg.SetDefaults()
	if cfg.Spec.SSH.PersistentSession == nil || !*cfg.Spec.SSH.PersistentSession {
		t.Fatal("expected persistentSession to remain true when explicitly set")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/config/ -run TestSetDefaultsPersistentSession -v`
Expected: FAIL — `PersistentSession` field does not exist yet.

**Step 3: Add the field and helper**

In `internal/config/config.go`, add `PersistentSession` to `SSHSpec`:

```go
type SSHSpec struct {
	User              string `yaml:"user"`
	RemotePort        int    `yaml:"remotePort"`
	PrivateKeyPath    string `yaml:"privateKeyPath"`
	AutoDetectPorts   *bool  `yaml:"autoDetectPorts"`
	PersistentSession *bool  `yaml:"persistentSession"`
	KeepAliveInterval int    `yaml:"keepAliveIntervalSeconds"`
	KeepAliveTimeout  int    `yaml:"keepAliveTimeoutSeconds"`
}
```

Add a helper method:

```go
func (s SSHSpec) PersistentSessionEnabled() bool {
	if s.PersistentSession == nil {
		return false
	}
	return *s.PersistentSession
}
```

No change to `SetDefaults` — nil means false (off by default), unlike `AutoDetectPorts` which defaults to true.

**Step 4: Run test to verify it passes**

Run: `go test ./internal/config/ -run TestSetDefaultsPersistentSession -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/config/config.go internal/config/config_test.go
git commit -m "feat(config): add persistentSession field to SSHSpec"
```

---

### Task 2: Pass `OKDEV_TMUX` env var to sidecar container

**Files:**
- Modify: `internal/kube/podspec.go:13` (PreparePodSpec signature + sidecar env)
- Test: `internal/kube/podspec_test.go`

**Step 1: Write the failing tests**

Add to `internal/kube/podspec_test.go`:

```go
func TestPreparePodSpecTmuxEnvEnabled(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", true)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			sidecar = c
			break
		}
	}
	found := false
	for _, e := range sidecar.Env {
		if e.Name == "OKDEV_TMUX" && e.Value == "1" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected OKDEV_TMUX=1 env var on okdev-sidecar when tmux enabled")
	}
}

func TestPreparePodSpecTmuxEnvDisabled(t *testing.T) {
	spec, err := PreparePodSpec(corev1.PodSpec{}, "ws-pvc", "/workspace", "ghcr.io/acmore/okdev-sidecar:edge", false)
	if err != nil {
		t.Fatal(err)
	}
	var sidecar corev1.Container
	for _, c := range spec.Containers {
		if c.Name == "okdev-sidecar" {
			sidecar = c
			break
		}
	}
	for _, e := range sidecar.Env {
		if e.Name == "OKDEV_TMUX" {
			t.Fatal("expected no OKDEV_TMUX env var on okdev-sidecar when tmux disabled")
		}
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/kube/ -run TestPreparePodSpecTmux -v`
Expected: FAIL — signature mismatch (too many arguments).

**Step 3: Add `tmux` parameter to `PreparePodSpec`**

In `internal/kube/podspec.go`, update the signature and sidecar container creation:

```go
func PreparePodSpec(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath, sidecarImage string, tmux bool) (corev1.PodSpec, error) {
```

In the sidecar container block (inside the `if !hasContainer` block), add env conditionally:

```go
		sidecarContainer := corev1.Container{
			Name:            "okdev-sidecar",
			Image:           sidecarImage,
			ImagePullPolicy: syncthingImagePullPolicy(sidecarImage),
			SecurityContext: &corev1.SecurityContext{
				Privileged: &privileged,
			},
			Ports: []corev1.ContainerPort{
				{ContainerPort: 22, Name: "ssh"},
				{ContainerPort: 8384, Name: "st-gui"},
				{ContainerPort: 22000, Name: "st-sync"},
			},
			VolumeMounts: []corev1.VolumeMount{
				{Name: "workspace", MountPath: workspaceMountPath},
				{Name: "syncthing-home", MountPath: "/var/syncthing"},
			},
		}
		if tmux {
			sidecarContainer.Env = append(sidecarContainer.Env, corev1.EnvVar{
				Name:  "OKDEV_TMUX",
				Value: "1",
			})
		}
		spec.Containers = append(spec.Containers, sidecarContainer)
```

**Step 4: Fix existing callers and tests**

Update all existing `PreparePodSpec` calls to add `false` as the last argument:

- `internal/cli/up.go:70` — change to `kube.PreparePodSpec(..., false)` (will be wired properly in Task 3)
- All existing tests in `internal/kube/podspec_test.go` — add `false` as the 5th argument to every `PreparePodSpec(...)` call.

**Step 5: Run all tests to verify they pass**

Run: `go test ./internal/kube/ -v`
Expected: ALL PASS

**Step 6: Commit**

```bash
git add internal/kube/podspec.go internal/kube/podspec_test.go internal/cli/up.go
git commit -m "feat(kube): pass OKDEV_TMUX env var to sidecar container"
```

---

### Task 3: Add `--tmux` flag to `okdev up`

**Files:**
- Modify: `internal/cli/up.go:17-154`

**Step 1: Add flag and wire to PreparePodSpec**

In `internal/cli/up.go`, add a `tmux` bool var alongside the existing flags:

```go
var waitTimeout time.Duration
var dryRun bool
var tmux bool
```

Resolve the effective tmux value (flag overrides config):

```go
// After cfg is loaded and defaults set, before pod creation:
enableTmux := tmux || cfg.Spec.SSH.PersistentSessionEnabled()
```

Pass to `PreparePodSpec`:

```go
preparedSpec, err := kube.PreparePodSpec(
    cfg.Spec.PodTemplate.Spec,
    pvc,
    cfg.Spec.Workspace.MountPath,
    cfg.Spec.Sidecar.Image,
    enableTmux,
)
```

Add the flag registration:

```go
cmd.Flags().BoolVar(&tmux, "tmux", false, "Enable tmux persistent shell sessions in the sidecar")
```

**Step 2: Build to verify compilation**

Run: `go build ./internal/cli/`
Expected: Success, no errors.

**Step 3: Commit**

```bash
git add internal/cli/up.go
git commit -m "feat(cli): add --tmux flag to okdev up"
```

---

### Task 4: Add `--no-tmux` flag to `okdev ssh`

**Files:**
- Modify: `internal/cli/ssh.go:23-118` (newSSHCmd function)

**Step 1: Add flag and send env var**

In `newSSHCmd`, add a `noTmux` bool var:

```go
var noTmux bool
```

Before calling `tm.OpenShell()` or `tm.Exec()`, if `noTmux` is set, send the env var via the SSH session. However, since `OpenShell` and `Exec` don't currently support env vars, and the `ForceCommand` script checks environment, we need to use `SSH_ORIGINAL_COMMAND` or `SendRequest` to set env.

The simplest approach: when `--no-tmux` is set and no `--cmd` is provided, run the shell as a command instead of using `OpenShell`. Change the shell invocation:

```go
if cmdStr == "" && noTmux {
    cmdStr = "OKDEV_NO_TMUX=1 exec $SHELL -l"
}
```

Wait — this won't work because `ForceCommand` intercepts before the command runs. The `SSH_ORIGINAL_COMMAND` env var is what `nsenter-dev.sh` checks. When `--no-tmux` is used, we can set a command that signals the intent:

Actually, the cleanest approach is to use SSH env passing. Add `AcceptEnv OKDEV_NO_TMUX` to `sshd_config`, and use `s.Setenv("OKDEV_NO_TMUX", "1")` in Go before starting the shell. But `x/crypto/ssh` `Session.Setenv` requires server-side `AcceptEnv` config.

Simpler: when `--no-tmux` is set, run the command as `SSH_ORIGINAL_COMMAND` by using `Exec` with a shell command, which bypasses the interactive tmux path in `nsenter-dev.sh`:

```go
if cmdStr != "" {
    out, err := tm.Exec(cmdStr)
    // ... existing handling
} else if noTmux {
    // Run interactive shell bypassing tmux by using exec with a login shell
    out, err := tm.Exec("exec bash -l || exec sh -l")
    // ... but this won't be interactive
}
```

This won't work for interactive use. Better approach: **use SSH `Setenv`**.

In `internal/cli/ssh.go`, modify `OpenShell` path:

The most robust approach is to accept `OKDEV_NO_TMUX` in `sshd_config` and use `Session.Setenv`:

1. Add `AcceptEnv OKDEV_NO_TMUX` to `infra/sidecar/sshd_config`
2. In `OpenShell`, add an optional env-setting step

But `OpenShell` is in the `ssh` package and has no env support. Let's add a `SetEnv` map to `TunnelManager` or add an `OpenShellWithEnv` variant.

**Simplest working approach:** Add an `Env` field to `TunnelManager` that `OpenShell` sends before starting the shell.

In `internal/ssh/tunnel.go`, add to `TunnelManager`:

```go
type TunnelManager struct {
    // ... existing fields
    Env map[string]string // environment variables to set on new sessions
}
```

In `OpenShell`, after creating the session and before calling `s.Shell()`:

```go
for k, v := range tm.Env {
    // Best-effort; server must AcceptEnv for this to work
    _ = s.Setenv(k, v)
}
```

In `internal/cli/ssh.go`, when `--no-tmux` is set:

```go
if noTmux {
    if tm.Env == nil {
        tm.Env = map[string]string{}
    }
    tm.Env["OKDEV_NO_TMUX"] = "1"
}
```

Add flag:

```go
cmd.Flags().BoolVar(&noTmux, "no-tmux", false, "Disable tmux for this SSH session even if enabled on the pod")
```

**Step 2: Update sshd_config**

In `infra/sidecar/sshd_config`, add:

```
AcceptEnv OKDEV_NO_TMUX
```

**Step 3: Build to verify compilation**

Run: `go build ./internal/cli/ && go build ./internal/ssh/`
Expected: Success

**Step 4: Commit**

```bash
git add internal/cli/ssh.go internal/ssh/tunnel.go infra/sidecar/sshd_config
git commit -m "feat(ssh): add --no-tmux flag for per-connection tmux opt-out"
```

---

### Task 5: Update sidecar entrypoint to wrap interactive shells in tmux

**Files:**
- Modify: `infra/sidecar/entrypoint.sh:65-71` (interactive shell section of nsenter-dev.sh)

**Step 1: Update the interactive shell section**

In `infra/sidecar/entrypoint.sh`, replace the interactive login shell block (lines 65-70) inside the heredoc:

```bash
# Interactive login shell in dev container.
if [ "$OKDEV_TMUX" = "1" ] && [ "${OKDEV_NO_TMUX:-}" != "1" ] && command -v tmux >/dev/null 2>&1; then
  if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- tmux new-session -A -s okdev "bash -l"
  else
    exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- tmux new-session -A -s okdev "sh -l"
  fi
fi
if nsenter --target "$DEV_PID" --mount -- test -x /bin/bash 2>/dev/null; then
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/bash -l
else
  exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -l
fi
```

This does:
- Checks `OKDEV_TMUX=1` (pod-level opt-in)
- Checks `OKDEV_NO_TMUX` is not `1` (per-connection opt-out)
- Checks `tmux` is available (graceful fallback)
- Uses `tmux new-session -A -s okdev` to create or attach
- Falls back to bare shell if any condition fails

**Step 2: Verify the script is syntactically valid**

Run: `sh -n infra/sidecar/entrypoint.sh`
Expected: No output (valid syntax)

**Step 3: Commit**

```bash
git add infra/sidecar/entrypoint.sh
git commit -m "feat(sidecar): wrap interactive SSH shells in tmux when OKDEV_TMUX=1"
```

---

### Task 6: Install tmux in sidecar Docker image

**Files:**
- Modify: `infra/sidecar/Dockerfile:6` (apk add line)

**Step 1: Add tmux to the package list**

In `infra/sidecar/Dockerfile`, line 6, add `tmux` to the `apk add` command:

```dockerfile
RUN apk add --no-cache openssh-server ca-certificates curl tar util-linux tmux \
```

**Step 2: Build to verify**

Run: `docker build -t okdev-sidecar-test infra/sidecar/`
Expected: Successful build (skip if no Docker available locally)

**Step 3: Commit**

```bash
git add infra/sidecar/Dockerfile
git commit -m "feat(sidecar): install tmux in sidecar image"
```

---

### Task 7: Add `AcceptEnv` to sshd_config

**Files:**
- Modify: `infra/sidecar/sshd_config`

**Step 1: Add AcceptEnv directive**

Add after the `ClientAliveCountMax` line in `infra/sidecar/sshd_config`:

```
AcceptEnv OKDEV_NO_TMUX
```

**Step 2: Commit**

```bash
git add infra/sidecar/sshd_config
git commit -m "feat(sidecar): accept OKDEV_NO_TMUX env var in sshd"
```

---

### Summary of changes by file

| File | Change |
|------|--------|
| `internal/config/config.go` | Add `PersistentSession *bool` to `SSHSpec`, add `PersistentSessionEnabled()` helper |
| `internal/config/config_test.go` | Add tests for `PersistentSession` defaults |
| `internal/kube/podspec.go` | Add `tmux bool` param to `PreparePodSpec`, set `OKDEV_TMUX=1` env on sidecar |
| `internal/kube/podspec_test.go` | Add tests for tmux env var, update existing test signatures |
| `internal/cli/up.go` | Add `--tmux` flag, resolve effective value, pass to `PreparePodSpec` |
| `internal/cli/ssh.go` | Add `--no-tmux` flag, set `OKDEV_NO_TMUX=1` in `TunnelManager.Env` |
| `internal/ssh/tunnel.go` | Add `Env map[string]string` field, send env vars in `OpenShell` |
| `infra/sidecar/Dockerfile` | Add `tmux` to `apk add` |
| `infra/sidecar/sshd_config` | Add `AcceptEnv OKDEV_NO_TMUX` |
| `infra/sidecar/entrypoint.sh` | Wrap interactive shells in tmux when `OKDEV_TMUX=1` |
