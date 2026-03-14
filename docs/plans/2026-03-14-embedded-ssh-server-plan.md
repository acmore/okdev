# Embedded SSH Server Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Add an okteto-style embedded Go SSH server that runs inside the dev container, activated via `--ssh-mode embedded` flag on `okdev up`.

**Architecture:** A static Go SSH binary (`okdev-sshd`) is built with `gliderlabs/ssh` and baked into the sidecar image. At pod startup, the sidecar copies the binary into the dev container and starts it via nsenter. SSH traffic goes directly to this server (port 2222) in the dev container's cgroup, bypassing the sidecar's OpenSSH + nsenter-per-session chain.

**Tech Stack:** Go 1.25, `github.com/gliderlabs/ssh`, `github.com/creack/pty`, `github.com/pkg/sftp`

---

### Task 1: Build the `okdev-sshd` binary

**Files:**
- Create: `cmd/okdev-sshd/main.go`

**Step 1: Add dependencies**

Run:
```bash
go get github.com/gliderlabs/ssh@latest github.com/creack/pty@latest github.com/pkg/sftp@latest
```

**Step 2: Write the SSH server binary**

Create `cmd/okdev-sshd/main.go`:

```go
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/creack/pty"
	"github.com/gliderlabs/ssh"
	"github.com/pkg/sftp"
	flag "github.com/spf13/pflag"
)

func main() {
	port := flag.Int("port", 2222, "SSH listen port")
	authorizedKeysPath := flag.String("authorized-keys", "/var/okdev/authorized_keys", "Path to authorized_keys file")
	shell := flag.String("shell", "", "Shell to use (auto-detect if empty)")
	flag.Parse()

	if *shell == "" {
		*shell = detectShell()
	}

	keys, err := loadAuthorizedKeys(*authorizedKeysPath)
	if err != nil {
		log.Fatalf("failed to load authorized keys: %v", err)
	}

	srv := &ssh.Server{
		Addr:    fmt.Sprintf(":%d", *port),
		Handler: sessionHandler(*shell),
		SubsystemHandlers: map[string]ssh.SubsystemHandler{
			"sftp": sftpHandler,
		},
	}

	if keys != nil {
		srv.PublicKeyHandler = func(ctx ssh.Context, key ssh.PublicKey) bool {
			for _, k := range keys {
				if ssh.KeysEqual(key, key) {
					return true
				}
			}
			return false
		}
	}

	log.Printf("okdev-sshd listening on :%d", *port)
	log.Fatal(srv.ListenAndServe())
}

func detectShell() string {
	for _, sh := range []string{"/bin/bash", "/bin/sh"} {
		if _, err := os.Stat(sh); err == nil {
			return sh
		}
	}
	return "/bin/sh"
}

func loadAuthorizedKeys(path string) ([]ssh.PublicKey, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var keys []ssh.PublicKey
	for len(data) > 0 {
		pubKey, _, _, rest, err := ssh.ParseAuthorizedKey(data)
		if err != nil {
			return nil, err
		}
		keys = append(keys, pubKey)
		data = rest
	}
	return keys, nil
}

func sessionHandler(shell string) ssh.Handler {
	return func(s ssh.Session) {
		cmd := buildCmd(s, shell)

		ptyReq, winCh, isPty := s.Pty()
		if isPty {
			if err := handlePTY(cmd, s, ptyReq, winCh); err != nil {
				exitCode := exitStatus(err)
				_ = s.Exit(exitCode)
				return
			}
			_ = s.Exit(0)
			return
		}

		if err := handleNoPTY(cmd, s); err != nil {
			exitCode := exitStatus(err)
			_ = s.Exit(exitCode)
			return
		}
		_ = s.Exit(0)
	}
}

func buildCmd(s ssh.Session, shell string) *exec.Cmd {
	var cmd *exec.Cmd
	if len(s.RawCommand()) == 0 {
		cmd = exec.Command(shell, "-l")
	} else {
		cmd = exec.Command(shell, "-lc", s.RawCommand())
	}
	cmd.Env = append(os.Environ(), s.Environ()...)
	return cmd
}

func handlePTY(cmd *exec.Cmd, s ssh.Session, ptyReq ssh.Pty, winCh <-chan ssh.Window) error {
	if ptyReq.Term != "" {
		cmd.Env = append(cmd.Env, "TERM="+ptyReq.Term)
	}
	f, err := pty.Start(cmd)
	if err != nil {
		return err
	}
	defer f.Close()

	go func() {
		for win := range winCh {
			setWinsize(f, win.Width, win.Height)
		}
	}()

	go io.Copy(f, s)

	done := make(chan struct{})
	go func() {
		defer close(done)
		io.Copy(s, f)
	}()

	if err := cmd.Wait(); err != nil {
		return err
	}
	select {
	case <-done:
	case <-time.After(1 * time.Second):
	}
	return nil
}

func handleNoPTY(cmd *exec.Cmd, s ssh.Session) error {
	stdout, _ := cmd.StdoutPipe()
	stderr, _ := cmd.StderrPipe()
	stdin, _ := cmd.StdinPipe()

	if err := cmd.Start(); err != nil {
		return err
	}

	var wg sync.WaitGroup
	wg.Add(3)
	go func() { defer wg.Done(); io.Copy(stdin, s); stdin.Close() }()
	go func() { defer wg.Done(); io.Copy(s, stdout) }()
	go func() { defer wg.Done(); io.Copy(s.Stderr(), stderr) }()
	wg.Wait()

	return cmd.Wait()
}

func setWinsize(f *os.File, w, h int) {
	syscall.Syscall(syscall.SYS_IOCTL, f.Fd(), uintptr(syscall.TIOCSWINSZ),
		uintptr(unsafe.Pointer(&struct{ h, w, x, y uint16 }{uint16(h), uint16(w), 0, 0})))
}

func exitStatus(err error) int {
	if err == nil {
		return 0
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		if ws, ok := exitErr.Sys().(syscall.WaitStatus); ok {
			return ws.ExitStatus()
		}
	}
	return 1
}

func sftpHandler(s ssh.Session) {
	server, err := sftp.NewServer(s)
	if err != nil {
		log.Printf("sftp server init error: %v", err)
		return
	}
	if err := server.Serve(); err == io.EOF {
		server.Close()
	}
}
```

**Step 3: Fix the PublicKeyHandler bug and build**

Note: The `PublicKeyHandler` above has a bug (`ssh.KeysEqual(key, key)` compares key to itself). Fix it to `ssh.KeysEqual(k, key)`.

Run:
```bash
CGO_ENABLED=0 go build -o /tmp/okdev-sshd ./cmd/okdev-sshd/
```
Expected: builds successfully

**Step 4: Commit**

```bash
git add cmd/okdev-sshd/ go.mod go.sum
git commit -m "feat(sshd): add embedded Go SSH server binary"
```

---

### Task 2: Add `okdev-sshd` to sidecar Dockerfile

**Files:**
- Modify: `infra/sidecar/Dockerfile`

**Step 1: Add multi-stage Go build**

Add a build stage before the Alpine stage:

```dockerfile
FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /okdev-sshd ./cmd/okdev-sshd/
```

Then in the Alpine stage, copy the binary:

```dockerfile
COPY --from=builder /okdev-sshd /usr/local/bin/okdev-sshd
```

**Step 2: Verify Dockerfile builds**

Run:
```bash
docker build -f infra/sidecar/Dockerfile -t okdev-sidecar-test .
```
Expected: builds successfully

**Step 3: Commit**

```bash
git add infra/sidecar/Dockerfile
git commit -m "build(sidecar): add okdev-sshd binary to sidecar image"
```

---

### Task 3: Add `--ssh-mode` flag to `okdev up`

**Files:**
- Modify: `internal/cli/up.go`
- Modify: `internal/kube/podspec.go`
- Modify: `internal/kube/podspec_test.go`

**Step 1: Write failing test for podspec with ssh-mode env var**

In `internal/kube/podspec_test.go`, add:

```go
func TestPreparePodSpecEmbeddedSSHMode(t *testing.T) {
	spec := corev1.PodSpec{}
	result, err := PreparePodSpec(spec, nil, "/workspace", "ghcr.io/acmore/okdev:test", false, false, "")
	if err != nil {
		t.Fatal(err)
	}
	sidecar := findContainer(result.Containers, "okdev-sidecar")
	if sidecar == nil {
		t.Fatal("sidecar not found")
	}
	for _, e := range sidecar.Env {
		if e.Name == "OKDEV_SSH_MODE" {
			t.Fatal("expected no OKDEV_SSH_MODE env var when embeddedSSH disabled")
		}
	}

	result2, err := PreparePodSpec(spec, nil, "/workspace", "ghcr.io/acmore/okdev:test", false, true, "")
	if err != nil {
		t.Fatal(err)
	}
	sidecar2 := findContainer(result2.Containers, "okdev-sidecar")
	if sidecar2 == nil {
		t.Fatal("sidecar not found")
	}
	found := false
	for _, e := range sidecar2.Env {
		if e.Name == "OKDEV_SSH_MODE" && e.Value == "embedded" {
			found = true
		}
	}
	if !found {
		t.Fatal("expected OKDEV_SSH_MODE=embedded env var on okdev-sidecar when embeddedSSH enabled")
	}
	// Check port 2222 is exposed
	hasPort := false
	for _, p := range sidecar2.Ports {
		if p.ContainerPort == 2222 && p.Name == "embedded-ssh" {
			hasPort = true
		}
	}
	if !hasPort {
		t.Fatal("expected port 2222 on sidecar when embeddedSSH enabled")
	}
}
```

**Step 2: Run test to verify it fails**

Run: `go test ./internal/kube/ -run TestPreparePodSpecEmbeddedSSHMode -v`
Expected: FAIL (wrong number of args to PreparePodSpec)

**Step 3: Add `embeddedSSH` parameter to `PreparePodSpec`**

Modify `internal/kube/podspec.go`:

- Add `embeddedSSH bool` parameter to `PreparePodSpec` function signature (after `tmux bool`)
- When `embeddedSSH` is true:
  - Add `OKDEV_SSH_MODE=embedded` env var to sidecar container
  - Add port `{ContainerPort: 2222, Name: "embedded-ssh"}` to sidecar ports

**Step 4: Update all callers of `PreparePodSpec`**

In `internal/cli/up.go`, add `embeddedSSH` parameter to the call:

```go
preparedSpec, err := kube.PreparePodSpec(
    cfg.Spec.PodTemplate.Spec,
    volumes,
    cfg.WorkspaceMountPath(),
    cfg.Spec.Sidecar.Image,
    enableTmux,
    sshMode == "embedded",
    preStopCmd,
)
```

Add `--ssh-mode` flag:
```go
cmd.Flags().StringVar(&sshMode, "ssh-mode", "sidecar", "SSH server mode: sidecar (default) or embedded")
```

Declare `sshMode` variable alongside existing vars (`waitTimeout`, `dryRun`, `tmux`).

**Step 5: Fix existing tests that call `PreparePodSpec`**

Update all test calls to include the new `embeddedSSH bool` parameter (set to `false`).

**Step 6: Run tests**

Run: `go test ./internal/kube/ -v && go test ./internal/cli/ -v`
Expected: all PASS

**Step 7: Commit**

```bash
git add internal/kube/podspec.go internal/kube/podspec_test.go internal/cli/up.go
git commit -m "feat(cli): add --ssh-mode embedded flag to okdev up"
```

---

### Task 4: Update entrypoint.sh for embedded mode

**Files:**
- Modify: `infra/sidecar/entrypoint.sh`

**Step 1: Add embedded SSH startup logic**

After the `ForceCommand` config block and before syncthing, add:

```sh
# When embedded SSH mode is enabled, copy okdev-sshd into the dev container
# and start it there. The SSH server runs natively in the dev container's cgroup.
OKDEV_SSH_MODE="${OKDEV_SSH_MODE:-sidecar}"
if [ "$OKDEV_SSH_MODE" = "embedded" ]; then
  # Wait for the dev container to be ready (same PID discovery as nsenter-dev.sh)
  DEV_PID=""
  tries=0
  while [ -z "$DEV_PID" ] && [ "$tries" -lt 60 ]; do
    for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$' | sort -n); do
      [ "$pid" = "1" ] && continue
      [ "$pid" = "$$" ] && continue
      [ -r "/proc/$pid/root" ] 2>/dev/null || continue
      if ! [ "/proc/$pid/root" -ef "/proc/self/root" ] 2>/dev/null; then
        if [ -d "/proc/$pid" ]; then
          DEV_PID="$pid"
          break
        fi
      fi
    done
    if [ -z "$DEV_PID" ]; then
      sleep 0.5
      tries=$((tries + 1))
    fi
  done

  if [ -n "$DEV_PID" ]; then
    # Copy binary and authorized_keys into dev container
    nsenter --target "$DEV_PID" --mount -- mkdir -p /var/okdev
    cat /usr/local/bin/okdev-sshd | nsenter --target "$DEV_PID" --mount -- sh -c "cat > /var/okdev/okdev-sshd && chmod +x /var/okdev/okdev-sshd"
    cat /root/.ssh/authorized_keys | nsenter --target "$DEV_PID" --mount -- sh -c "cat > /var/okdev/authorized_keys && chmod 600 /var/okdev/authorized_keys"

    # Start okdev-sshd inside the dev container
    nsenter --target "$DEV_PID" --mount --uts --ipc --pid --cgroup -- \
      /var/okdev/okdev-sshd --port 2222 --authorized-keys /var/okdev/authorized_keys &
    echo "okdev-sshd started in dev container (PID $DEV_PID) on port 2222"
  else
    echo "WARNING: could not find dev container PID, embedded SSH not started" >&2
  fi
fi
```

**Step 2: Commit**

```bash
git add infra/sidecar/entrypoint.sh
git commit -m "feat(sidecar): inject and start okdev-sshd in dev container for embedded mode"
```

---

### Task 5: Route SSH connections to embedded server

**Files:**
- Modify: `internal/cli/ssh.go`
- Modify: `internal/cli/up.go`

**Step 1: Update `sshTargetContainer` for embedded mode**

When embedded mode is active, `ensureSSHKeyOnPod` should target the sidecar (where authorized_keys is initially written), but `waitForSSHDReady` should check the dev container.

Add a helper function to determine the remote SSH port:

```go
func sshRemotePort(cfg *config.DevEnvironment, sshMode string) int {
	if sshMode == "embedded" {
		return 2222
	}
	return cfg.Spec.SSH.RemotePort
}
```

**Step 2: Update `okdev up` SSH setup for embedded mode**

In `internal/cli/up.go`, when `sshMode == "embedded"`:
- Use remote port 2222 instead of `cfg.Spec.SSH.RemotePort` for `ensureSSHConfigEntry`
- Skip `waitForSSHDReady` (the entrypoint handles startup, and the sshd check looks for OpenSSH)
- Pass remote port 2222 to `ensureSSHConfigEntry` and `startManagedSSHForwardWithForwards`

**Step 3: Update `okdev ssh` for embedded mode**

In `newSSHCmd`, when `remotePort` resolves to the config value, check if the pod has embedded mode enabled. The simplest approach: store `sshMode` in a pod annotation (`okdev.io/ssh-mode`) during `okdev up`, then read it in `okdev ssh`.

Alternatively, keep it simple: if `remotePort == 0`, default to `cfg.Spec.SSH.RemotePort` (22) as before. Users who used `--ssh-mode embedded` during `okdev up` will have the SSH config entry pointing to port 2222 already via the ProxyCommand. The `okdev ssh` command reads the config and uses the right port.

**Step 4: Update `ssh-proxy` for embedded mode**

In `newSSHProxyCmd`, `remotePort` comes from the `--remote-port` flag which is set by the ProxyCommand in `~/.ssh/config`. Since `ensureSSHConfigEntry` writes the correct port, no changes needed here.

**Step 5: Run tests**

Run: `go build ./... && go test ./internal/cli/ -v`
Expected: all PASS

**Step 6: Commit**

```bash
git add internal/cli/ssh.go internal/cli/up.go
git commit -m "feat(cli): route SSH to embedded server when ssh-mode=embedded"
```

---

### Task 6: Add pod annotation for ssh-mode detection

**Files:**
- Modify: `internal/cli/up.go`

**Step 1: Annotate pod with ssh-mode**

After `k.Apply()` in `okdev up`, annotate the pod:

```go
if sshMode == "embedded" {
    if err := k.AnnotatePod(ctx, ns, pod, "okdev.io/ssh-mode", "embedded"); err != nil {
        slog.Debug("failed to annotate ssh-mode", "error", err)
    }
}
```

**Step 2: Read annotation in `okdev ssh`**

In `newSSHCmd`, after resolving remotePort, check annotation:

```go
if remotePort == 0 {
    annCtx, annCancel := context.WithTimeout(cmd.Context(), 5*time.Second)
    mode, _ := k.GetPodAnnotation(annCtx, ns, podName(sn), "okdev.io/ssh-mode")
    annCancel()
    if mode == "embedded" {
        remotePort = 2222
    } else {
        remotePort = cfg.Spec.SSH.RemotePort
    }
}
```

**Step 3: Run tests**

Run: `go build ./... && go test ./internal/cli/ -v`
Expected: all PASS

**Step 4: Commit**

```bash
git add internal/cli/up.go internal/cli/ssh.go
git commit -m "feat(cli): store and detect ssh-mode via pod annotation"
```

---

### Task 7: End-to-end manual test

**Steps:**

1. Build the sidecar image with the new Dockerfile:
   ```bash
   docker build -f infra/sidecar/Dockerfile -t ghcr.io/acmore/okdev:edge .
   ```

2. Build the CLI:
   ```bash
   go build -o okdev .
   ```

3. Start with embedded mode:
   ```bash
   ./okdev up --ssh-mode embedded
   ```

4. Verify:
   - `ssh okdev-<session>` connects successfully
   - `okdev ssh` connects successfully
   - Run a memory-heavy command — it should use the dev container's limits
   - File transfer via SFTP works
   - Port forwards work
   - Disconnect and reconnect works

5. Verify sidecar mode still works:
   ```bash
   ./okdev down && ./okdev up
   ssh okdev-<session>
   ```

**No commit needed — this is manual verification.**
