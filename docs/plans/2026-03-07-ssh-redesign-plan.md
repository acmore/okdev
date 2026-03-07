# SSH Redesign Implementation Plan

> **For Claude:** REQUIRED SUB-SKILL: Use superpowers:executing-plans to implement this plan task-by-task.

**Goal:** Replace the shell-out-to-ssh approach with an in-process Go SSH tunnel manager, merged sidecar, nsenter-based dev container access, and auto-detect port forwarding.

**Architecture:** A new `internal/ssh` package provides a `TunnelManager` using `golang.org/x/crypto/ssh` that manages the persistent SSH connection, port forwards, and auto-detect. The sidecar image merges syncthing + sshd into one container. SSH sessions enter the dev container via `nsenter` through shared PID namespace.

**Tech Stack:** Go, `golang.org/x/crypto/ssh`, Kubernetes client-go, Alpine Linux (sidecar image)

**Design doc:** `docs/plans/2026-03-07-ssh-redesign-design.md`

---

### Task 1: Add `golang.org/x/crypto` Dependency

**Files:**
- Modify: `go.mod`
- Modify: `go.sum`

**Step 1: Add the dependency**

Run: `cd /Users/acmore/workspace/okdev && go get golang.org/x/crypto/ssh`

**Step 2: Verify import works**

Run: `go build ./...`
Expected: builds successfully

**Step 3: Commit**

```bash
git add go.mod go.sum
git commit -m "deps: add golang.org/x/crypto for native SSH client"
```

---

### Task 2: Merge Sidecar Dockerfile

Replace separate `infra/sshd/` and `infra/syncthing/` with a single `infra/sidecar/`.

**Files:**
- Create: `infra/sidecar/Dockerfile`
- Create: `infra/sidecar/sshd_config`
- Create: `infra/sidecar/entrypoint.sh`
- Delete: `infra/sshd/` (after verifying new sidecar works)
- Delete: `infra/syncthing/` (after verifying new sidecar works)

**Step 1: Create the merged Dockerfile**

```dockerfile
FROM alpine:3.21

ARG SYNCTHING_VERSION=v1.29.7

# Install openssh-server + syncthing dependencies
RUN apk add --no-cache openssh-server ca-certificates curl tar util-linux \
    && mkdir -p /run/sshd /root/.ssh \
    && chmod 700 /root/.ssh

# Install syncthing
RUN arch="$(uname -m)" \
    && case "$arch" in \
         x86_64) syncthing_arch="amd64" ;; \
         aarch64) syncthing_arch="arm64" ;; \
         *) echo "unsupported architecture: $arch" >&2; exit 1 ;; \
       esac \
    && os="linux" \
    && archive="syncthing-${os}-${syncthing_arch}-${SYNCTHING_VERSION}.tar.gz" \
    && base_url="https://github.com/syncthing/syncthing/releases/download/${SYNCTHING_VERSION}" \
    && curl -fsSL "${base_url}/sha256sum.txt.asc" -o /tmp/sha256sum.txt \
    && curl -fsSL "${base_url}/${archive}" -o "/tmp/${archive}" \
    && (cd /tmp && grep " ${archive}$" sha256sum.txt | sha256sum -c -) \
    && tar -xzf "/tmp/${archive}" -C /tmp \
    && install -m 0755 "/tmp/syncthing-${os}-${syncthing_arch}-${SYNCTHING_VERSION}/syncthing" /usr/local/bin/syncthing \
    && rm -rf /tmp/syncthing* /tmp/sha256sum.txt

COPY sshd_config /etc/ssh/sshd_config
COPY entrypoint.sh /entrypoint.sh
RUN chmod +x /entrypoint.sh

EXPOSE 22 8384 22000

ENTRYPOINT ["/entrypoint.sh"]
```

**Step 2: Create sshd_config**

Same as existing `infra/sshd/sshd_config` but with nsenter ForceCommand:

```
Port 22
Protocol 2
HostKey /etc/ssh/ssh_host_rsa_key
HostKey /etc/ssh/ssh_host_ecdsa_key
HostKey /etc/ssh/ssh_host_ed25519_key
PermitRootLogin prohibit-password
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
UsePAM no
PubkeyAuthentication yes
AuthorizedKeysFile .ssh/authorized_keys
ClientAliveInterval 30
ClientAliveCountMax 10
PidFile /run/sshd/sshd.pid
Subsystem sftp internal-sftp
```

Note: `ForceCommand` with nsenter will be handled by the entrypoint script that writes a wrapper, since the target PID is dynamic. See Task 3 for details.

**Step 3: Create entrypoint.sh**

```bash
#!/bin/sh
set -eu

# Setup SSH
mkdir -p /run/sshd /root/.ssh
chmod 700 /root/.ssh
touch /root/.ssh/authorized_keys
chmod 600 /root/.ssh/authorized_keys

if [ ! -f /etc/ssh/ssh_host_ed25519_key ]; then
  ssh-keygen -A
fi

# Write nsenter wrapper script for SSH sessions.
# Finds PID 1 of the dev container (first process whose root is not our root).
cat > /usr/local/bin/nsenter-dev.sh << 'SCRIPT'
#!/bin/sh
# Find the dev container's PID 1 by looking for a process with a different root filesystem.
# In a shared PID namespace, our PID 1 is the pause/sandbox container.
# We look for the first "sleep infinity" or the init process of the dev container.
DEV_PID=""
for pid in $(ls /proc | grep -E '^[0-9]+$' | sort -n); do
  [ "$pid" = "1" ] && continue
  [ "$pid" = "$$" ] && continue
  # Skip if we can't read the process
  [ -r "/proc/$pid/root" ] || continue
  # Check if this process has a different root than us (different container)
  if ! [ "/proc/$pid/root" -ef "/proc/self/root" ]; then
    # Found a process in a different mount namespace — likely the dev container
    DEV_PID="$pid"
    break
  fi
done

if [ -z "$DEV_PID" ]; then
  echo "ERROR: could not find dev container PID" >&2
  exit 1
fi

exec nsenter --target "$DEV_PID" --mount --uts --ipc --pid -- /bin/sh -l
SCRIPT
chmod +x /usr/local/bin/nsenter-dev.sh

# Add ForceCommand to sshd_config dynamically
if ! grep -q "ForceCommand" /etc/ssh/sshd_config; then
  echo "ForceCommand /usr/local/bin/nsenter-dev.sh" >> /etc/ssh/sshd_config
fi

# Start syncthing in background (run as root for workspace access)
syncthing serve --home /var/syncthing --no-browser \
  --gui-address=http://0.0.0.0:8384 --no-restart --skip-port-probing &

# Start sshd in foreground
exec /usr/sbin/sshd -D -e -f /etc/ssh/sshd_config
```

**Step 4: Verify build locally**

Run: `cd /Users/acmore/workspace/okdev && docker build -t okdev-sidecar-test infra/sidecar/`
Expected: builds successfully

**Step 5: Commit**

```bash
git add infra/sidecar/
git commit -m "infra: add merged sidecar (sshd + syncthing + nsenter)"
```

Note: Old `infra/sshd/` and `infra/syncthing/` directories are kept until migration is complete and tested, then removed in a later cleanup task.

---

### Task 3: Update Pod Spec for Merged Sidecar + Shared PID Namespace

**Files:**
- Modify: `internal/kube/podspec.go:12-94`
- Modify: `internal/kube/podspec_test.go`

**Step 1: Write failing tests**

Add test cases to `internal/kube/podspec_test.go`:

```go
func TestPreparePodSpecWithSidecar_MergedContainer(t *testing.T) {
    spec := corev1.PodSpec{
        Containers: []corev1.Container{{
            Name:  "dev",
            Image: "ubuntu:22.04",
        }},
    }
    result, err := PreparePodSpec(spec, "ws-pvc", "/workspace", true, "ghcr.io/acmore/okdev:edge")
    if err != nil {
        t.Fatal(err)
    }
    // Should have shared PID namespace enabled
    if result.ShareProcessNamespace == nil || !*result.ShareProcessNamespace {
        t.Error("expected ShareProcessNamespace to be true")
    }
    // Should have exactly 2 containers: dev + okdev-sidecar
    if len(result.Containers) != 2 {
        t.Fatalf("expected 2 containers, got %d", len(result.Containers))
    }
    sidecar := result.Containers[1]
    if sidecar.Name != "okdev-sidecar" {
        t.Errorf("expected sidecar name 'okdev-sidecar', got %q", sidecar.Name)
    }
    // Sidecar should expose ports 22, 8384, 22000
    ports := map[int32]bool{}
    for _, p := range sidecar.Ports {
        ports[p.ContainerPort] = true
    }
    for _, expected := range []int32{22, 8384, 22000} {
        if !ports[expected] {
            t.Errorf("sidecar missing port %d", expected)
        }
    }
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/kube/ -run TestPreparePodSpecWithSidecar_MergedContainer -v`
Expected: FAIL

**Step 3: Refactor PreparePodSpec to create merged sidecar**

Replace `PreparePodSpecWithSSH` with an updated `PreparePodSpec` that always creates the merged sidecar:

```go
func PreparePodSpec(podSpec corev1.PodSpec, workspaceClaim, workspaceMountPath string, syncthingEnabled bool, sidecarImage string) (corev1.PodSpec, error) {
    spec := podSpec.DeepCopy()
    if spec == nil {
        spec = &corev1.PodSpec{}
    }
    if len(spec.Containers) == 0 {
        spec.Containers = []corev1.Container{{
            Name:    "dev",
            Image:   "ubuntu:22.04",
            Command: []string{"sleep", "infinity"},
        }}
    }

    // Enable shared PID namespace for nsenter into dev container
    shareProcess := true
    spec.ShareProcessNamespace = &shareProcess

    spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
        Name: "workspace",
        VolumeSource: corev1.VolumeSource{
            PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: workspaceClaim},
        },
    })

    for i := range spec.Containers {
        spec.Containers[i].VolumeMounts = ensureVolumeMount(spec.Containers[i].VolumeMounts, corev1.VolumeMount{
            Name:      "workspace",
            MountPath: workspaceMountPath,
        })
    }

    // Always add merged sidecar (syncthing + sshd)
    if sidecarImage == "" {
        return corev1.PodSpec{}, fmt.Errorf("sidecar image cannot be empty")
    }
    if !hasContainer(spec.Containers, "okdev-sidecar") {
        spec.Volumes = ensureVolume(spec.Volumes, corev1.Volume{
            Name:         "syncthing-home",
            VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
        })
        spec.Containers = append(spec.Containers, corev1.Container{
            Name:            "okdev-sidecar",
            Image:           sidecarImage,
            ImagePullPolicy: sidecarImagePullPolicy(sidecarImage),
            Ports: []corev1.ContainerPort{
                {ContainerPort: 22, Name: "ssh"},
                {ContainerPort: 8384, Name: "st-gui"},
                {ContainerPort: 22000, Name: "st-sync"},
            },
            VolumeMounts: []corev1.VolumeMount{
                {Name: "workspace", MountPath: workspaceMountPath},
                {Name: "syncthing-home", MountPath: "/var/syncthing"},
            },
        })
    }

    return *spec, nil
}
```

Remove `PreparePodSpecWithSSH` — it's no longer needed.

**Step 4: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/kube/ -v`
Expected: PASS (update existing tests that reference old function signatures)

**Step 5: Update callers**

Update `internal/cli/up.go` lines 71-79 to call the simplified `PreparePodSpec`:

```go
preparedSpec, err := kube.PreparePodSpec(
    cfg.Spec.PodTemplate.Spec,
    pvc,
    cfg.Spec.Workspace.MountPath,
    cfg.Spec.Sync.Engine == "syncthing",
    sidecarImage(cfg),
)
```

Where `sidecarImage(cfg)` resolves from the new config field `spec.sidecar.image`.

**Step 6: Run full test suite**

Run: `cd /Users/acmore/workspace/okdev && go test ./... -v`
Expected: PASS

**Step 7: Commit**

```bash
git add internal/kube/podspec.go internal/kube/podspec_test.go internal/cli/up.go
git commit -m "refactor: merge syncthing+ssh into single okdev-sidecar container"
```

---

### Task 4: Update Config — Remove Old SSH Fields, Add New Ones

**Files:**
- Modify: `internal/config/config.go:12-258`
- Modify: `internal/config/config_test.go`

**Step 1: Write failing test for new config shape**

```go
func TestSSHSpecDefaults_NewShape(t *testing.T) {
    cfg := &DevEnvironment{
        APIVersion: "okdev.io/v1alpha1",
        Kind:       "DevEnvironment",
        Metadata:   Metadata{Name: "test"},
        Spec: DevEnvSpec{
            Workspace: Workspace{MountPath: "/workspace"},
            Sync:      SyncSpec{Engine: "syncthing"},
        },
    }
    cfg.SetDefaults()
    if cfg.Spec.SSH.User != "root" {
        t.Errorf("expected user root, got %q", cfg.Spec.SSH.User)
    }
    if cfg.Spec.SSH.RemotePort != 22 {
        t.Errorf("expected remotePort 22, got %d", cfg.Spec.SSH.RemotePort)
    }
    if cfg.Spec.SSH.AutoDetectPorts != true {
        t.Error("expected autoDetectPorts true by default")
    }
    if cfg.Spec.Sidecar.Image == "" {
        t.Error("expected sidecar image to have a default")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/config/ -run TestSSHSpecDefaults_NewShape -v`
Expected: FAIL (AutoDetectPorts and Sidecar fields don't exist)

**Step 3: Update config types**

In `internal/config/config.go`:

```go
// Remove DefaultSSHImageRepository, DefaultSSHImageFallback, DefaultSSHMode constants
// Keep DefaultSyncthingImageRepository renamed for sidecar

const (
    DefaultSyncthingVersion         = "v1.29.7"
    DefaultSidecarImageRepository   = "ghcr.io/acmore/okdev"
    DefaultSidecarImageFallback     = "edge"
    DefaultWorkspacePVCSize         = "50Gi"
)

type DevEnvSpec struct {
    Namespace   string         `yaml:"namespace"`
    Session     SessionSpec    `yaml:"session"`
    Workspace   Workspace      `yaml:"workspace"`
    Sync        SyncSpec       `yaml:"sync"`
    Ports       []PortMapping  `yaml:"ports"`
    SSH         SSHSpec        `yaml:"ssh"`
    Sidecar     SidecarSpec    `yaml:"sidecar"`
    PodTemplate PodTemplateRef `yaml:"podTemplate"`
}

type SSHSpec struct {
    User            string `yaml:"user"`
    RemotePort      int    `yaml:"remotePort"`
    PrivateKeyPath  string `yaml:"privateKeyPath"`
    AutoDetectPorts *bool  `yaml:"autoDetectPorts"`
}

type SidecarSpec struct {
    Image string `yaml:"image"`
}
```

Update `SetDefaults()`:

```go
if d.Spec.SSH.User == "" {
    d.Spec.SSH.User = "root"
}
if d.Spec.SSH.RemotePort == 0 {
    d.Spec.SSH.RemotePort = 22
}
if d.Spec.SSH.AutoDetectPorts == nil {
    v := true
    d.Spec.SSH.AutoDetectPorts = &v
}
if d.Spec.Sidecar.Image == "" {
    d.Spec.Sidecar.Image = DefaultSidecarImageForBinaryVersion(version.Version)
}
```

Update `Validate()` — remove mode/localPort/sidecarImage validations, add sidecar.image validation.

**Step 4: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/config/ -v`
Expected: PASS (fix any existing tests that reference removed fields)

**Step 5: Fix all compilation errors from removed fields**

Grep for `SSH.Mode`, `SSH.LocalPort`, `SSH.SidecarImage` across the codebase and update callers.

Run: `cd /Users/acmore/workspace/okdev && go build ./...`
Expected: builds successfully

**Step 6: Commit**

```bash
git add internal/config/
git commit -m "config: simplify SSH spec, add sidecar.image and autoDetectPorts"
```

---

### Task 5: Implement `internal/ssh` Package — TunnelManager Core

**Files:**
- Create: `internal/ssh/tunnel.go`
- Create: `internal/ssh/tunnel_test.go`

**Step 1: Write failing test for Connect**

```go
package ssh

import (
    "context"
    "testing"
)

func TestTunnelManager_ConnectFailsWithBadAddress(t *testing.T) {
    tm := &TunnelManager{
        SSHUser:    "root",
        SSHKeyPath: "/nonexistent/key",
        RemotePort: 22,
    }
    err := tm.Connect(context.Background(), "127.0.0.1", 0)
    if err == nil {
        t.Fatal("expected error connecting to invalid address")
    }
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -run TestTunnelManager_ConnectFailsWithBadAddress -v`
Expected: FAIL (package doesn't exist)

**Step 3: Implement TunnelManager core**

Create `internal/ssh/tunnel.go`:

```go
package ssh

import (
    "context"
    "fmt"
    "io"
    "log/slog"
    "net"
    "os"
    "sync"
    "time"

    "golang.org/x/crypto/ssh"
    "golang.org/x/term"
)

// TunnelManager manages a persistent SSH connection with port forwarding.
type TunnelManager struct {
    SSHUser    string
    SSHKeyPath string
    RemotePort int

    mu     sync.Mutex
    client *ssh.Client
    ctx    context.Context
    cancel context.CancelFunc

    forwards     []Forward
    autoForwards []Forward
    listeners    map[int]net.Listener // local port -> listener
}

type Forward struct {
    LocalPort  int
    RemotePort int
    Auto       bool // true if auto-detected
}

// Connect establishes an SSH connection to the given local address (typically
// the local end of a kubectl port-forward).
func (tm *TunnelManager) Connect(ctx context.Context, host string, port int) error {
    key, err := os.ReadFile(tm.SSHKeyPath)
    if err != nil {
        return fmt.Errorf("read ssh key: %w", err)
    }
    signer, err := ssh.ParsePrivateKey(key)
    if err != nil {
        return fmt.Errorf("parse ssh key: %w", err)
    }

    config := &ssh.ClientConfig{
        User: tm.SSHUser,
        Auth: []ssh.AuthMethod{
            ssh.PublicKeys(signer),
        },
        HostKeyCallback: ssh.InsecureIgnoreHostKey(),
        Timeout:         10 * time.Second,
    }

    addr := fmt.Sprintf("%s:%d", host, port)
    client, err := ssh.Dial("tcp", addr, config)
    if err != nil {
        return fmt.Errorf("ssh dial %s: %w", addr, err)
    }

    tm.mu.Lock()
    tm.client = client
    tm.ctx, tm.cancel = context.WithCancel(ctx)
    if tm.listeners == nil {
        tm.listeners = make(map[int]net.Listener)
    }
    tm.mu.Unlock()

    return nil
}

// Close tears down the SSH connection and all port forwards.
func (tm *TunnelManager) Close() error {
    tm.mu.Lock()
    defer tm.mu.Unlock()

    if tm.cancel != nil {
        tm.cancel()
    }
    for port, ln := range tm.listeners {
        ln.Close()
        delete(tm.listeners, port)
    }
    if tm.client != nil {
        err := tm.client.Close()
        tm.client = nil
        return err
    }
    return nil
}

// IsConnected returns true if the SSH client is connected.
func (tm *TunnelManager) IsConnected() bool {
    tm.mu.Lock()
    defer tm.mu.Unlock()
    return tm.client != nil
}

// AddForward starts a local port forward over the SSH connection.
func (tm *TunnelManager) AddForward(localPort, remotePort int) error {
    tm.mu.Lock()
    client := tm.client
    tm.mu.Unlock()

    if client == nil {
        return fmt.Errorf("not connected")
    }

    ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
    if err != nil {
        return fmt.Errorf("listen on local port %d: %w", localPort, err)
    }

    tm.mu.Lock()
    tm.listeners[localPort] = ln
    tm.mu.Unlock()

    go tm.serveForward(ln, client, localPort, remotePort)
    return nil
}

func (tm *TunnelManager) serveForward(ln net.Listener, client *ssh.Client, localPort, remotePort int) {
    for {
        conn, err := ln.Accept()
        if err != nil {
            return // listener closed
        }
        go func() {
            defer conn.Close()
            remote, err := client.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
            if err != nil {
                slog.Debug("forward dial failed", "remotePort", remotePort, "error", err)
                return
            }
            defer remote.Close()
            bidirectionalCopy(conn, remote)
        }()
    }
}

func bidirectionalCopy(a, b net.Conn) {
    var wg sync.WaitGroup
    wg.Add(2)
    go func() {
        defer wg.Done()
        io.Copy(a, b)
    }()
    go func() {
        defer wg.Done()
        io.Copy(b, a)
    }()
    wg.Wait()
}

// RemoveForward stops a local port forward.
func (tm *TunnelManager) RemoveForward(localPort int) {
    tm.mu.Lock()
    defer tm.mu.Unlock()
    if ln, ok := tm.listeners[localPort]; ok {
        ln.Close()
        delete(tm.listeners, localPort)
    }
}

// OpenShell opens an interactive shell session over SSH.
func (tm *TunnelManager) OpenShell() error {
    tm.mu.Lock()
    client := tm.client
    tm.mu.Unlock()

    if client == nil {
        return fmt.Errorf("not connected")
    }

    session, err := client.NewSession()
    if err != nil {
        return fmt.Errorf("new session: %w", err)
    }
    defer session.Close()

    session.Stdin = os.Stdin
    session.Stdout = os.Stdout
    session.Stderr = os.Stderr

    fd := int(os.Stdin.Fd())
    if term.IsTerminal(fd) {
        oldState, err := term.MakeRaw(fd)
        if err != nil {
            return fmt.Errorf("set terminal raw: %w", err)
        }
        defer term.Restore(fd, oldState)

        w, h, _ := term.GetSize(fd)
        if err := session.RequestPty("xterm-256color", h, w, ssh.TerminalModes{
            ssh.ECHO:          1,
            ssh.TTY_OP_ISPEED: 14400,
            ssh.TTY_OP_OSPEED: 14400,
        }); err != nil {
            return fmt.Errorf("request pty: %w", err)
        }
    }

    if err := session.Shell(); err != nil {
        return fmt.Errorf("start shell: %w", err)
    }
    return session.Wait()
}

// Exec runs a command over SSH and returns the output.
func (tm *TunnelManager) Exec(cmd string) ([]byte, error) {
    tm.mu.Lock()
    client := tm.client
    tm.mu.Unlock()

    if client == nil {
        return nil, fmt.Errorf("not connected")
    }

    session, err := client.NewSession()
    if err != nil {
        return nil, fmt.Errorf("new session: %w", err)
    }
    defer session.Close()

    return session.CombinedOutput(cmd)
}
```

**Step 4: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/ssh/
git commit -m "feat(ssh): add TunnelManager core with connect, forward, shell, exec"
```

---

### Task 6: Implement Reconnect Loop

**Files:**
- Modify: `internal/ssh/tunnel.go`
- Create: `internal/ssh/reconnect.go`
- Create: `internal/ssh/reconnect_test.go`

**Step 1: Write failing test**

```go
func TestReconnectLoop_CallsConnectOnDisconnect(t *testing.T) {
    connectCalls := 0
    connectFn := func() error {
        connectCalls++
        if connectCalls < 3 {
            return fmt.Errorf("not ready")
        }
        return nil
    }
    err := reconnectWithBackoff(context.Background(), connectFn, 3, 10*time.Millisecond, 50*time.Millisecond)
    if err != nil {
        t.Fatalf("expected success after retries, got: %v", err)
    }
    if connectCalls != 3 {
        t.Errorf("expected 3 connect calls, got %d", connectCalls)
    }
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -run TestReconnectLoop -v`
Expected: FAIL

**Step 3: Implement reconnect logic**

Create `internal/ssh/reconnect.go`:

```go
package ssh

import (
    "context"
    "fmt"
    "log/slog"
    "time"
)

// reconnectWithBackoff retries connectFn with exponential backoff.
// Returns nil on success, error if maxRetries exceeded or context cancelled.
func reconnectWithBackoff(ctx context.Context, connectFn func() error, maxRetries int, initialBackoff, maxBackoff time.Duration) error {
    backoff := initialBackoff
    for i := 0; i < maxRetries; i++ {
        select {
        case <-ctx.Done():
            return ctx.Err()
        default:
        }
        if err := connectFn(); err != nil {
            slog.Debug("reconnect attempt failed", "attempt", i+1, "error", err)
            select {
            case <-ctx.Done():
                return ctx.Err()
            case <-time.After(backoff):
            }
            backoff *= 2
            if backoff > maxBackoff {
                backoff = maxBackoff
            }
            continue
        }
        return nil
    }
    return fmt.Errorf("failed to reconnect after %d attempts", maxRetries)
}

// StartKeepAlive monitors the SSH connection and triggers reconnect on failure.
// portForwardFn is called to re-establish the kubectl port-forward before SSH reconnect.
// connectFn re-establishes the SSH connection + all forwards.
func (tm *TunnelManager) StartKeepAlive(portForwardFn func() (int, func(), error), connectAndForwardFn func(localPort int) error) {
    go func() {
        ticker := time.NewTicker(10 * time.Second)
        defer ticker.Stop()
        for {
            select {
            case <-tm.ctx.Done():
                return
            case <-ticker.C:
                tm.mu.Lock()
                client := tm.client
                tm.mu.Unlock()
                if client == nil {
                    continue
                }
                // Send keepalive
                _, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
                if err != nil {
                    slog.Info("SSH connection lost, reconnecting...")
                    tm.Close()

                    err := reconnectWithBackoff(tm.ctx, func() error {
                        localPort, cancelPF, pfErr := portForwardFn()
                        if pfErr != nil {
                            return pfErr
                        }
                        if err := connectAndForwardFn(localPort); err != nil {
                            cancelPF()
                            return err
                        }
                        return nil
                    }, 0, 1*time.Second, 30*time.Second) // 0 = unlimited retries
                    if err != nil {
                        slog.Error("reconnect failed permanently", "error", err)
                        return
                    }
                    slog.Info("Reconnected successfully")
                }
            }
        }
    }()
}
```

Note: `maxRetries=0` means unlimited retries (keep trying until context cancelled). Update `reconnectWithBackoff` to support this:

```go
func reconnectWithBackoff(ctx context.Context, connectFn func() error, maxRetries int, initialBackoff, maxBackoff time.Duration) error {
    backoff := initialBackoff
    for i := 0; maxRetries == 0 || i < maxRetries; i++ {
        // ...
    }
    // ...
}
```

**Step 4: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -v`
Expected: PASS

**Step 5: Commit**

```bash
git add internal/ssh/reconnect.go internal/ssh/reconnect_test.go
git commit -m "feat(ssh): add reconnect loop with exponential backoff and keepalive"
```

---

### Task 7: Implement Auto-Detect Port Forwarding

**Files:**
- Create: `internal/ssh/autodetect.go`
- Create: `internal/ssh/autodetect_test.go`

**Step 1: Write failing test for port parsing**

```go
func TestParseSS_ExtractsListeningPorts(t *testing.T) {
    ssOutput := `State  Recv-Q Send-Q Local Address:Port  Peer Address:Port
LISTEN 0      128          0.0.0.0:3000       0.0.0.0:*
LISTEN 0      128          0.0.0.0:8080       0.0.0.0:*
LISTEN 0      128        127.0.0.1:9229       0.0.0.0:*
LISTEN 0      128             [::]:22            [::]:*
`
    excluded := map[int]bool{22: true, 8384: true, 22000: true}
    ports := parseListeningPorts(ssOutput, excluded)
    expected := []int{3000, 8080, 9229}
    if len(ports) != len(expected) {
        t.Fatalf("expected %d ports, got %d: %v", len(expected), len(ports), ports)
    }
    for i, p := range expected {
        if ports[i] != p {
            t.Errorf("port[%d]: expected %d, got %d", i, p, ports[i])
        }
    }
}
```

**Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -run TestParseSS -v`
Expected: FAIL

**Step 3: Implement auto-detect**

Create `internal/ssh/autodetect.go`:

```go
package ssh

import (
    "log/slog"
    "regexp"
    "sort"
    "strconv"
    "strings"
    "time"
)

var excludedPorts = map[int]bool{
    22:    true, // sshd
    8384:  true, // syncthing GUI
    22000: true, // syncthing sync
}

// parseListeningPorts extracts listening port numbers from `ss -tlnp` output.
func parseListeningPorts(ssOutput string, excluded map[int]bool) []int {
    portSet := map[int]bool{}
    // Match lines like "LISTEN 0 128 0.0.0.0:3000 0.0.0.0:*"
    re := regexp.MustCompile(`LISTEN\s+\d+\s+\d+\s+\S+:(\d+)\s+`)
    for _, match := range re.FindAllStringSubmatch(ssOutput, -1) {
        port, err := strconv.Atoi(match[1])
        if err != nil || port <= 0 || port > 65535 {
            continue
        }
        if excluded[port] {
            continue
        }
        portSet[port] = true
    }
    ports := make([]int, 0, len(portSet))
    for p := range portSet {
        ports = append(ports, p)
    }
    sort.Ints(ports)
    return ports
}

// parseProcNetTCP is a fallback parser for /proc/net/tcp when ss is not available.
func parseProcNetTCP(content string, excluded map[int]bool) []int {
    portSet := map[int]bool{}
    for _, line := range strings.Split(content, "\n") {
        fields := strings.Fields(line)
        if len(fields) < 4 {
            continue
        }
        // State 0A = LISTEN
        if fields[3] != "0A" {
            continue
        }
        parts := strings.Split(fields[1], ":")
        if len(parts) != 2 {
            continue
        }
        port64, err := strconv.ParseInt(parts[1], 16, 32)
        if err != nil || port64 <= 0 || port64 > 65535 {
            continue
        }
        port := int(port64)
        if excluded[port] {
            continue
        }
        portSet[port] = true
    }
    ports := make([]int, 0, len(portSet))
    for p := range portSet {
        ports = append(ports, p)
    }
    sort.Ints(ports)
    return ports
}

// StartAutoDetect periodically scans for listening ports in the dev container
// and auto-forwards new ones. Removes forwards for ports that stop listening.
func (tm *TunnelManager) StartAutoDetect(interval time.Duration, configuredPorts map[int]bool, onAdd func(int), onRemove func(int)) {
    excluded := make(map[int]bool)
    for p := range excludedPorts {
        excluded[p] = true
    }
    for p := range configuredPorts {
        excluded[p] = true
    }

    go func() {
        ticker := time.NewTicker(interval)
        defer ticker.Stop()
        known := map[int]bool{}

        for {
            select {
            case <-tm.ctx.Done():
                return
            case <-ticker.C:
                ports := tm.detectListeningPorts(excluded)
                if ports == nil {
                    continue
                }
                current := map[int]bool{}
                for _, p := range ports {
                    current[p] = true
                }
                // Add new ports
                for _, p := range ports {
                    if !known[p] {
                        if err := tm.AddForward(p, p); err != nil {
                            slog.Debug("auto-forward failed", "port", p, "error", err)
                            continue
                        }
                        known[p] = true
                        if onAdd != nil {
                            onAdd(p)
                        }
                    }
                }
                // Remove stale ports
                for p := range known {
                    if !current[p] {
                        tm.RemoveForward(p)
                        delete(known, p)
                        if onRemove != nil {
                            onRemove(p)
                        }
                    }
                }
            }
        }
    }()
}

func (tm *TunnelManager) detectListeningPorts(excluded map[int]bool) []int {
    // Try ss first
    out, err := tm.Exec("ss -tlnp 2>/dev/null || cat /proc/net/tcp 2>/dev/null")
    if err != nil {
        return nil
    }
    output := string(out)
    if strings.Contains(output, "LISTEN") {
        return parseListeningPorts(output, excluded)
    }
    return parseProcNetTCP(output, excluded)
}
```

**Step 4: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -v`
Expected: PASS

**Step 5: Write test for /proc/net/tcp fallback**

```go
func TestParseProcNetTCP_ExtractsListeningPorts(t *testing.T) {
    // Port 3000 = 0BB8, Port 8080 = 1F90
    content := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000:0BB8 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12345 1
   1: 00000000:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12346 1
   2: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000     0        0 12347 1
`
    excluded := map[int]bool{22: true}
    ports := parseProcNetTCP(content, excluded)
    expected := []int{3000, 8080}
    if len(ports) != len(expected) {
        t.Fatalf("expected %d ports, got %d: %v", len(expected), len(ports), ports)
    }
}
```

**Step 6: Run tests**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/ssh/ -v`
Expected: PASS

**Step 7: Commit**

```bash
git add internal/ssh/autodetect.go internal/ssh/autodetect_test.go
git commit -m "feat(ssh): add auto-detect port forwarding with ss and /proc/net/tcp fallback"
```

---

### Task 8: Rewrite `okdev ssh` Command

**Files:**
- Modify: `internal/cli/ssh.go`

**Step 1: Rewrite newSSHCmd to use TunnelManager**

```go
func newSSHCmd(opts *Options) *cobra.Command {
    var user string
    var remotePort int
    var keyPath string
    var setupKey bool
    var cmdStr string

    cmd := &cobra.Command{
        Use:   "ssh",
        Short: "Connect to dev container over SSH",
        RunE: func(cmd *cobra.Command, args []string) error {
            cfg, ns, err := loadConfigAndNamespace(opts)
            if err != nil {
                return err
            }
            sn, err := resolveSessionName(opts, cfg)
            if err != nil {
                return err
            }
            k := newKubeClient(opts)
            if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
                return err
            }
            stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), true, true)
            defer stopMaintenance()

            if user == "" {
                user = cfg.Spec.SSH.User
            }
            if remotePort == 0 {
                remotePort = cfg.Spec.SSH.RemotePort
            }
            if keyPath == "" {
                keyPath, err = defaultSSHKeyPath(cfg)
                if err != nil {
                    return err
                }
            }
            if setupKey {
                if err := ensureSSHKeyOnPod(opts, cfg, ns, podName(sn), keyPath); err != nil {
                    return err
                }
            }

            // Start kubectl port-forward to sidecar SSH port
            cancelPF, localPort, err := startPortForwardToSSH(k, ns, podName(sn), remotePort)
            if err != nil {
                return err
            }
            defer cancelPF()

            tm := &okdevssh.TunnelManager{
                SSHUser:    user,
                SSHKeyPath: keyPath,
                RemotePort: remotePort,
            }
            if err := tm.Connect(cmd.Context(), "127.0.0.1", localPort); err != nil {
                return fmt.Errorf("ssh connect: %w", err)
            }
            defer tm.Close()

            if cmdStr != "" {
                out, err := tm.Exec(cmdStr)
                if err != nil {
                    return fmt.Errorf("ssh exec: %w", err)
                }
                fmt.Fprint(cmd.OutOrStdout(), string(out))
                return nil
            }
            return tm.OpenShell()
        },
    }

    cmd.Flags().StringVar(&user, "user", "", "SSH username")
    cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote SSH port in pod")
    cmd.Flags().StringVar(&keyPath, "key", "", "Private key path")
    cmd.Flags().BoolVar(&setupKey, "setup-key", false, "Generate and install SSH key in pod")
    cmd.Flags().StringVar(&cmdStr, "cmd", "", "Run a command over SSH")
    return cmd
}

// startPortForwardToSSH starts a kubectl port-forward to the sidecar SSH port.
// Returns cancel func, actual local port, and error.
func startPortForwardToSSH(k interface {
    PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}, namespace, pod string, remotePort int) (context.CancelFunc, int, error) {
    localPort, err := reserveEphemeralPort()
    if err != nil {
        return nil, 0, fmt.Errorf("reserve ephemeral port: %w", err)
    }
    cancel, pfErr := startManagedPortForwardNoProbeWithClient(k, namespace, pod, []string{fmt.Sprintf("%d:%d", localPort, remotePort)})
    if pfErr != nil {
        return nil, 0, pfErr
    }
    // Wait for port-forward to be ready
    deadline := time.Now().Add(10 * time.Second)
    for time.Now().Before(deadline) {
        conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 500*time.Millisecond)
        if err == nil {
            conn.Close()
            return cancel, localPort, nil
        }
        time.Sleep(200 * time.Millisecond)
    }
    cancel()
    return nil, 0, fmt.Errorf("port-forward to pod SSH not ready within 10s")
}
```

**Step 2: Remove old SSH functions that are no longer needed**

Remove from `ssh.go`:
- `startSSHPortForwardWithFallback()`
- `isPortListenConflict()`
- `startManagedSSHForward()`
- `stopManagedSSHForward()`
- `sshControlSocketPath()`
- `sshTargetContainer()`
- `waitDialLocal()`

Keep:
- `ensureSSHKeyOnPod()` (still needed)
- `defaultSSHKeyPath()` (still needed)
- `ensureSSHConfigEntry()` (updated — see Task 9)
- `stripManagedSSHBlock()` (still needed)
- `shellQuote()` (still needed)
- `sshHostAlias()` (still needed)
- `reserveEphemeralPort()` (still needed)

**Step 3: Update ensureSSHKeyOnPod to always target the sidecar**

```go
func ensureSSHKeyOnPod(opts *Options, cfg *config.DevEnvironment, namespace, pod, keyPath string) error {
    // ... (key generation stays the same) ...

    k := newKubeClient(opts)
    container := "okdev-sidecar" // always target merged sidecar
    var lastErr error
    for i := 0; i < 3; i++ {
        ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
        _, err = k.ExecShInContainer(ctx, namespace, pod, container, script)
        cancel()
        if err == nil {
            return nil
        }
        lastErr = err
        time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
    }
    return fmt.Errorf("install ssh key in pod: %w", lastErr)
}
```

**Step 4: Verify compilation**

Run: `cd /Users/acmore/workspace/okdev && go build ./...`
Expected: builds

**Step 5: Commit**

```bash
git add internal/cli/ssh.go
git commit -m "refactor(ssh): rewrite okdev ssh to use Go SSH TunnelManager"
```

---

### Task 9: Rewrite `okdev ssh-proxy` and SSH Config Entry

**Files:**
- Modify: `internal/cli/ssh.go` (ssh-proxy command and ensureSSHConfigEntry)

**Step 1: Update ensureSSHConfigEntry — remove LocalForward lines**

```go
func ensureSSHConfigEntry(hostAlias, sessionName, user string, remotePort int, keyPath, okdevConfigPath string) error {
    home, err := os.UserHomeDir()
    if err != nil {
        return err
    }
    sshDir := filepath.Join(home, ".ssh")
    if err := os.MkdirAll(sshDir, 0o700); err != nil {
        return err
    }
    sshConfigPath := filepath.Join(sshDir, "config")
    existing, _ := os.ReadFile(sshConfigPath)

    begin := "# BEGIN OKDEV " + hostAlias
    end := "# END OKDEV " + hostAlias
    proxyInner := fmt.Sprintf("okdev --session %s ssh-proxy --remote-port %d", shellQuote(sessionName), remotePort)
    if strings.TrimSpace(okdevConfigPath) != "" {
        proxyInner += " -c " + shellQuote(okdevConfigPath)
    }
    proxyCmd := "sh -lc " + shellQuote(proxyInner)
    blockLines := []string{
        begin,
        "Host " + hostAlias,
        "  HostName 127.0.0.1",
        "  User " + user,
        "  IdentityFile " + keyPath,
        "  StrictHostKeyChecking no",
        "  UserKnownHostsFile /dev/null",
        "  ProxyCommand " + proxyCmd,
        "  ServerAliveInterval 30",
        "  ServerAliveCountMax 10",
        end,
    }
    block := strings.Join(blockLines, "\n") + "\n"
    updated := stripManagedSSHBlock(string(existing), begin, end) + "\n" + block
    updated = strings.TrimSpace(updated) + "\n"
    return os.WriteFile(sshConfigPath, []byte(updated), 0o600)
}
```

**Step 2: Rewrite ssh-proxy to use Go SSH**

```go
func newSSHProxyCmd(opts *Options) *cobra.Command {
    var remotePort int
    cmd := &cobra.Command{
        Use:    "ssh-proxy",
        Short:  "Internal SSH proxy bridge",
        Hidden: true,
        RunE: func(cmd *cobra.Command, args []string) error {
            cfg, ns, err := loadConfigAndNamespace(opts)
            if err != nil {
                return err
            }
            sn, err := resolveSessionName(opts, cfg)
            if err != nil {
                return err
            }
            k := newKubeClient(opts)
            if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
                return err
            }
            if remotePort == 0 {
                remotePort = cfg.Spec.SSH.RemotePort
            }

            cancelPF, localPort, err := startPortForwardToSSH(k, ns, podName(sn), remotePort)
            if err != nil {
                return err
            }
            defer cancelPF()

            // Bridge stdin/stdout to the SSH port (not SSH protocol — raw TCP for ProxyCommand)
            conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 10*time.Second)
            if err != nil {
                return fmt.Errorf("dial ssh port: %w", err)
            }
            defer conn.Close()

            var wg sync.WaitGroup
            var copyErr error
            var once sync.Once
            setErr := func(err error) {
                if err == nil || errors.Is(err, io.EOF) {
                    return
                }
                once.Do(func() { copyErr = err })
            }
            wg.Add(2)
            go func() {
                defer wg.Done()
                _, err := io.Copy(conn, os.Stdin)
                setErr(err)
                if cw, ok := conn.(interface{ CloseWrite() error }); ok {
                    _ = cw.CloseWrite()
                }
            }()
            go func() {
                defer wg.Done()
                _, err := io.Copy(os.Stdout, conn)
                setErr(err)
            }()
            wg.Wait()
            return copyErr
        },
    }
    cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote SSH port in pod")
    return cmd
}
```

**Step 3: Update callers of ensureSSHConfigEntry (in up.go)**

Remove `localPort` and `forwards` parameters from the call.

**Step 4: Verify compilation**

Run: `cd /Users/acmore/workspace/okdev && go build ./...`
Expected: builds

**Step 5: Commit**

```bash
git add internal/cli/ssh.go internal/cli/up.go
git commit -m "refactor(ssh): simplify ssh-proxy and ssh config entry"
```

---

### Task 10: Rewrite `okdev up` SSH Integration

**Files:**
- Modify: `internal/cli/up.go`

**Step 1: Update the SSH setup section in okdev up**

Replace lines 110-136 with:

```go
// Setup SSH
keyPath, keyErr := defaultSSHKeyPath(cfg)
if keyErr != nil {
    fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to resolve SSH key path: %v\n", keyErr)
} else {
    if err := ensureSSHKeyOnPod(opts, cfg, ns, pod, keyPath); err != nil {
        fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to setup SSH key in pod: %v\n", err)
    }
    alias := sshHostAlias(sn)
    cfgPath, _ := config.ResolvePath(opts.ConfigPath)
    if cfgErr := ensureSSHConfigEntry(alias, sn, cfg.Spec.SSH.User, cfg.Spec.SSH.RemotePort, keyPath, cfgPath); cfgErr != nil {
        fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
    } else {
        fmt.Fprintf(cmd.OutOrStdout(), "SSH ready: ssh %s\n", alias)
    }

    // Start background TunnelManager for port forwards
    if err := startBackgroundTunnel(opts, cfg, ns, pod, sn, keyPath, cmd.OutOrStdout(), cmd.ErrOrStderr()); err != nil {
        fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to start tunnel manager: %v\n", err)
    }
}
```

**Step 2: Implement startBackgroundTunnel**

Add to `internal/cli/ssh.go` (or a new `internal/cli/tunnel.go`):

```go
func startBackgroundTunnel(opts *Options, cfg *config.DevEnvironment, ns, pod, sessionName, keyPath string, stdout, stderr io.Writer) error {
    k := newKubeClient(opts)

    cancelPF, localPort, err := startPortForwardToSSH(k, ns, pod, cfg.Spec.SSH.RemotePort)
    if err != nil {
        return err
    }

    tm := &okdevssh.TunnelManager{
        SSHUser:    cfg.Spec.SSH.User,
        SSHKeyPath: keyPath,
        RemotePort: cfg.Spec.SSH.RemotePort,
    }
    if err := tm.Connect(context.Background(), "127.0.0.1", localPort); err != nil {
        cancelPF()
        return err
    }

    // Add configured port forwards
    for _, p := range cfg.Spec.Ports {
        if p.Local <= 0 || p.Remote <= 0 {
            continue
        }
        if err := tm.AddForward(p.Local, p.Remote); err != nil {
            fmt.Fprintf(stderr, "warning: failed to forward port %d:%d: %v\n", p.Local, p.Remote, err)
        } else {
            fmt.Fprintf(stdout, "Forwarding port %d → %d\n", p.Local, p.Remote)
        }
    }

    // Start auto-detect if enabled
    if cfg.Spec.SSH.AutoDetectPorts == nil || *cfg.Spec.SSH.AutoDetectPorts {
        configuredPorts := map[int]bool{}
        for _, p := range cfg.Spec.Ports {
            configuredPorts[p.Remote] = true
        }
        tm.StartAutoDetect(2*time.Second, configuredPorts,
            func(port int) {
                fmt.Fprintf(stdout, "Auto-forwarded port %d (localhost:%d → pod:%d)\n", port, port, port)
            },
            func(port int) {
                fmt.Fprintf(stdout, "Port %d no longer listening, removed forward\n", port)
            },
        )
    }

    // Start keepalive/reconnect
    tm.StartKeepAlive(
        func() (int, func(), error) {
            return startPortForwardToSSHRaw(k, ns, pod, cfg.Spec.SSH.RemotePort)
        },
        func(lp int) error {
            if err := tm.Connect(context.Background(), "127.0.0.1", lp); err != nil {
                return err
            }
            // Re-add configured forwards
            for _, p := range cfg.Spec.Ports {
                if p.Local > 0 && p.Remote > 0 {
                    _ = tm.AddForward(p.Local, p.Remote)
                }
            }
            return nil
        },
    )

    // Save PID for cleanup by okdev down
    pidPath := tunnelPIDPath(sessionName)
    os.MkdirAll(filepath.Dir(pidPath), 0o700)
    os.WriteFile(pidPath, []byte(fmt.Sprintf("%d", os.Getpid())), 0o600)

    // Note: TunnelManager runs in this process. For okdev up to exit while
    // keeping the tunnel alive, we need to daemonize or use a long-running
    // process. For now, okdev up stays running (like okdev sync does).
    // The user can Ctrl+C to stop, and okdev down will clean up.

    return nil
}

func tunnelPIDPath(sessionName string) string {
    home, _ := os.UserHomeDir()
    return filepath.Join(home, ".okdev", "ssh", "okdev-"+sessionName+".pid")
}
```

**Step 3: Update okdev down to stop tunnel**

In `internal/cli/down.go`, replace `stopManagedSSHForward(alias)` with:

```go
_ = stopBackgroundTunnel(sn)
_ = removeSSHConfigEntry(alias)
```

Where `stopBackgroundTunnel` reads the PID file and sends SIGTERM:

```go
func stopBackgroundTunnel(sessionName string) error {
    pidPath := tunnelPIDPath(sessionName)
    data, err := os.ReadFile(pidPath)
    if err != nil {
        return nil // no tunnel running
    }
    pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
    if err != nil {
        os.Remove(pidPath)
        return nil
    }
    proc, err := os.FindProcess(pid)
    if err != nil {
        os.Remove(pidPath)
        return nil
    }
    _ = proc.Signal(os.Interrupt)
    os.Remove(pidPath)
    return nil
}
```

**Step 4: Verify compilation**

Run: `cd /Users/acmore/workspace/okdev && go build ./...`
Expected: builds

**Step 5: Commit**

```bash
git add internal/cli/ssh.go internal/cli/up.go internal/cli/down.go
git commit -m "feat(ssh): integrate TunnelManager into okdev up/down lifecycle"
```

---

### Task 11: Update Root Command Registration

**Files:**
- Modify: `internal/cli/root.go`

**Step 1: Verify command registration is correct**

Check that `newSSHCmd` and `newSSHProxyCmd` are still registered. Remove the `--local-port` flag from `ssh-proxy` if it was there.

**Step 2: Run full test suite**

Run: `cd /Users/acmore/workspace/okdev && go test ./... -v`
Expected: PASS (fix any remaining compilation/test errors)

**Step 3: Commit if changes needed**

```bash
git add internal/cli/root.go
git commit -m "chore: update command registration after SSH refactor"
```

---

### Task 12: Clean Up Old Infra and Config

**Files:**
- Delete: `infra/sshd/` (replaced by `infra/sidecar/`)
- Delete: `infra/syncthing/` (merged into `infra/sidecar/`)
- Modify: `internal/config/config.go` — remove `DefaultSSHImageRepository`, `DefaultSSHImageFallback`, `DefaultSSHMode`
- Modify: `.okdev.yaml` — update to new config shape

**Step 1: Remove old directories**

```bash
rm -rf infra/sshd/ infra/syncthing/
```

**Step 2: Update .okdev.yaml example**

```yaml
ssh:
  user: root
  remotePort: 22
  autoDetectPorts: true
sidecar:
  image: ghcr.io/acmore/okdev:edge
```

**Step 3: Run full build and tests**

Run: `cd /Users/acmore/workspace/okdev && go build ./... && go test ./...`
Expected: builds and passes

**Step 4: Commit**

```bash
git add -A
git commit -m "chore: remove old sshd/syncthing infra, update config to new shape"
```

---

### Task 13: Update Documentation

**Files:**
- Modify: `docs/command-reference.md`
- Modify: `docs/okdev-design.md`
- Modify: `docs/quickstart.md`

**Step 1: Update command-reference.md**

Update `okdev ssh` section:
- Remove `--local-port` flag
- Update description to mention Go SSH + dev container
- Document auto-detect port forwarding behavior

Update `okdev up` section:
- Document that it now starts persistent tunnel with auto-detect

**Step 2: Update okdev-design.md**

Add section on SSH architecture (TunnelManager, merged sidecar, nsenter).

**Step 3: Update quickstart.md**

Update SSH examples to reflect new behavior.

**Step 4: Commit**

```bash
git add docs/
git commit -m "docs: update for SSH redesign"
```

---

### Task 14: End-to-End Smoke Test

**No files to modify — manual verification.**

**Step 1: Build okdev**

Run: `cd /Users/acmore/workspace/okdev && go build -o okdev .`

**Step 2: Build and push sidecar image**

Run: `cd /Users/acmore/workspace/okdev && docker build -t ghcr.io/acmore/okdev:edge infra/sidecar/ && docker push ghcr.io/acmore/okdev:edge`

**Step 3: Test okdev up**

Run: `./okdev up`
Expected:
- Pod created with 2 containers (dev + okdev-sidecar)
- SSH key installed
- SSH config written
- Tunnel manager started
- Auto-detect running

**Step 4: Test okdev ssh**

Run: `./okdev ssh`
Expected: lands in dev container (not sidecar), can see workspace files

**Step 5: Test ssh via alias**

Run: `ssh okdev-<session>`
Expected: connects through ProxyCommand, lands in dev container

**Step 6: Test port forwarding**

In dev container: `python3 -m http.server 3000`
Locally: `curl http://localhost:3000`
Expected: auto-detected and forwarded within ~2 seconds

**Step 7: Test connection resilience**

Kill the kubectl port-forward process.
Expected: tunnel manager reconnects automatically, forwards resume.

**Step 8: Test okdev down**

Run: `./okdev down`
Expected: tunnel stopped, SSH config removed, pod deleted
