# SSH Proxy Health Monitoring and Keep-Alive Implementation Plan

> **For agentic workers:** REQUIRED: Use superpowers:subagent-driven-development (if subagents available) or superpowers:executing-plans to implement this plan. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make `okdev ssh-proxy` detect unresponsive connections and exit cleanly so users aren't stuck with frozen terminals.

**Architecture:** Three independent health-monitoring layers (TCP keepalive, data flow monitor, port-forward probe) each capable of detecting failure and closing the proxy connection. A sentinel error type suppresses stderr output in `main.go` while still exiting non-zero.

**Tech Stack:** Go stdlib (`net`, `sync/atomic`, `syscall`, `os/signal`), existing `startPortForwardKeepalive`

---

## File Structure

| File | Action | Responsibility |
|------|--------|---------------|
| `internal/cli/proxy_health.go` | Create | `monitoredCopy`, `proxyDataFlowWatchdog`, `setTCPKeepAliveProxyTuning`, `ErrProxyHealthDisconnect` sentinel |
| `internal/cli/proxy_health_test.go` | Create | Tests for monitoredCopy and watchdog |
| `internal/cli/ssh.go` | Modify | Wire health monitoring into `newSSHProxyCmd`, hardcode SSH config keepalive values |
| `internal/cli/ssh_config_test.go` | Modify | Add test for hardcoded keepalive values |
| `cmd/okdev/main.go` | Modify | Suppress sentinel error from stderr |

---

## Chunk 1: Data Flow Monitor and TCP Keepalive Tuning

### Task 1: Write `monitoredCopy` and watchdog tests

**Files:**
- Create: `internal/cli/proxy_health_test.go`

- [ ] **Step 1: Write the failing test for monitoredCopy timestamp tracking**

```go
package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestMonitoredCopyUpdatesTimestamp(t *testing.T) {
	var lastData atomic.Int64
	src := strings.NewReader("hello world")
	dst := &bytes.Buffer{}

	n, err := monitoredCopy(dst, src, &lastData)
	if err != nil {
		t.Fatalf("monitoredCopy error: %v", err)
	}
	if n != 11 {
		t.Fatalf("expected 11 bytes copied, got %d", n)
	}
	if dst.String() != "hello world" {
		t.Fatalf("unexpected output: %s", dst.String())
	}
	ts := lastData.Load()
	if ts == 0 {
		t.Fatal("expected lastData timestamp to be updated")
	}
	if time.Since(time.Unix(0, ts)) > time.Second {
		t.Fatal("timestamp too old")
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -run TestMonitoredCopyUpdatesTimestamp -v`
Expected: FAIL with "monitoredCopy not defined"

- [ ] **Step 3: Write the failing test for watchdog idle detection**

Add to `internal/cli/proxy_health_test.go`:

```go
func TestProxyDataFlowWatchdogClosesOnIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	// Accept a connection in background
	var serverConn net.Conn
	accepted := make(chan struct{})
	go func() {
		c, err := ln.Accept()
		if err == nil {
			serverConn = c
		}
		close(accepted)
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	<-accepted
	defer serverConn.Close()

	// Set lastData to a stale time (20s ago)
	var lastData atomic.Int64
	lastData.Store(time.Now().Add(-20 * time.Second).UnixNano())

	closed := make(chan struct{})
	go func() {
		proxyDataFlowWatchdog(ctx, conn, &lastData, 100*time.Millisecond, 500*time.Millisecond)
		close(closed)
	}()

	select {
	case <-closed:
		// Verify conn is actually closed by trying to write
		_, err := conn.Write([]byte("test"))
		if err == nil {
			t.Fatal("expected conn to be closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not close connection within timeout")
	}
}

func TestProxyDataFlowWatchdogDoesNotCloseActiveConn(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	accepted := make(chan net.Conn, 1)
	go func() {
		c, _ := ln.Accept()
		accepted <- c
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	serverConn := <-accepted
	defer serverConn.Close()

	// Set lastData to now (fresh)
	var lastData atomic.Int64
	lastData.Store(time.Now().UnixNano())

	watchdogDone := make(chan struct{})
	go func() {
		proxyDataFlowWatchdog(ctx, conn, &lastData, 100*time.Millisecond, 500*time.Millisecond)
		close(watchdogDone)
	}()

	// Wait a bit then cancel — watchdog should NOT have closed conn
	time.Sleep(300 * time.Millisecond)
	cancel()

	<-watchdogDone

	// Conn should still be writable
	_, err = conn.Write([]byte("test"))
	if err != nil {
		t.Fatalf("expected conn to still be open, got: %v", err)
	}
}

func TestMonitoredCopyPropagatesError(t *testing.T) {
	var lastData atomic.Int64
	dst := &bytes.Buffer{}
	errReader := &errorReader{err: io.ErrUnexpectedEOF}

	_, err := monitoredCopy(dst, errReader, &lastData)
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected ErrUnexpectedEOF, got: %v", err)
	}
}

type errorReader struct {
	err error
}

func (e *errorReader) Read(p []byte) (int, error) {
	return 0, e.err
}

func TestSetTCPKeepAliveProxyTuningSmoke(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() { c, _ := ln.Accept(); _ = c.Close() }()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	setTCPKeepAliveProxyTuning(conn) // should not panic
}
```

- [ ] **Step 4: Run tests to verify they fail**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -run 'TestProxyDataFlowWatchdog' -v`
Expected: FAIL

- [ ] **Step 5: Commit test file**

```bash
git add internal/cli/proxy_health_test.go
git commit -m "test: add failing tests for proxy data flow monitor"
```

### Task 2: Implement `monitoredCopy`, watchdog, TCP tuning, and sentinel error

**Files:**
- Create: `internal/cli/proxy_health.go`

- [ ] **Step 1: Write the implementation**

```go
package cli

import (
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"syscall"
	"time"

	"context"
)

// ErrProxyHealthDisconnect is a sentinel error returned when the proxy
// exits due to a health-check-triggered disconnection. It should be
// suppressed from stderr in main.go since the ssh client will report
// the disconnect naturally.
var ErrProxyHealthDisconnect = errors.New("proxy health disconnect")

// monitoredReader wraps an io.Reader and updates an atomic timestamp
// on every successful Read.
type monitoredReader struct {
	r        io.Reader
	lastData *atomic.Int64
}

func (m *monitoredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.lastData.Store(time.Now().UnixNano())
	}
	return n, err
}

// monitoredCopy copies from src to dst, updating lastData on every
// successful read. Returns bytes copied and any error.
func monitoredCopy(dst io.Writer, src io.Reader, lastData *atomic.Int64) (int64, error) {
	return io.Copy(dst, &monitoredReader{r: src, lastData: lastData})
}

// proxyDataFlowWatchdog checks lastData every checkInterval. If no data
// has flowed for idleThreshold, it closes conn and returns.
// It also returns when ctx is cancelled.
func proxyDataFlowWatchdog(ctx context.Context, conn net.Conn, lastData *atomic.Int64, checkInterval, idleThreshold time.Duration) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := lastData.Load()
			if last == 0 {
				continue // no data yet, skip
			}
			idle := time.Since(time.Unix(0, last))
			if idle >= idleThreshold {
				slog.Info("ssh-proxy data flow watchdog: idle timeout",
					"idle", idle.Round(time.Millisecond),
					"threshold", idleThreshold,
				)
				_ = conn.Close()
				return
			}
		}
	}
}

// setTCPKeepAliveProxyTuning reduces the TCP keepalive period and
// optionally sets TCP_KEEPCNT where supported.
func setTCPKeepAliveProxyTuning(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(5 * time.Second)

	// Try to set TCP_KEEPCNT=2 via syscall. This is best-effort.
	raw, err := tc.SyscallConn()
	if err != nil {
		slog.Debug("ssh-proxy: failed to get syscall conn for TCP_KEEPCNT", "error", err)
		return
	}
	var sysErr error
	err = raw.Control(func(fd uintptr) {
		sysErr = syscall.SetsockoptInt(int(fd), syscall.IPPROTO_TCP, syscall.TCP_KEEPCNT, 2)
	})
	if err != nil || sysErr != nil {
		slog.Debug("ssh-proxy: TCP_KEEPCNT not set (unsupported or failed)",
			"controlErr", err, "sysErr", sysErr)
	}
}
```

- [ ] **Step 2: Run all tests to verify they pass**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -run 'TestMonitoredCopy|TestProxyDataFlowWatchdog|TestSetTCPKeepAlive' -v`
Expected: PASS (all 5 tests)

- [ ] **Step 3: Commit**

```bash
git add internal/cli/proxy_health.go
git commit -m "feat: add proxy health monitoring utilities (monitoredCopy, watchdog, TCP tuning)"
```

---

## Chunk 2: Wire Health Monitoring into SSH Proxy Command

### Task 3: Wire three health layers into `newSSHProxyCmd`

**Files:**
- Modify: `internal/cli/ssh.go:517-600`

- [ ] **Step 1: Modify `newSSHProxyCmd` to use health monitoring**

Replace the proxy command body (lines 540-594) with:

```go
			cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(k, ns, podName(sn), 2222, remotePort)
			if err != nil {
				return err
			}
			if cancelPF != nil {
				defer cancelPF()
			}

			slog.Debug("ssh-proxy dialing local port-forward", "port", usedLocalPort)
			conn, err := waitDialLocal(usedLocalPort, 10*time.Second)
			if err != nil {
				slog.Debug("ssh-proxy dial failed", "error", err)
				return err
			}
			defer conn.Close()
			startTime := time.Now()

			// Layer 1: TCP keepalive tuning (5s period, KEEPCNT=2 where supported)
			setTCPKeepAliveProxyTuning(conn)

			// Layer 3: Port-forward keepalive probe with onDegraded wired up
			pfKeepAliveCtx, pfKeepAliveCancel := context.WithCancel(cmd.Context())
			defer pfKeepAliveCancel()
			startPortForwardKeepalive(pfKeepAliveCtx, usedLocalPort, 10*time.Second, func() {
				slog.Info("ssh-proxy port-forward degraded, closing connection",
					"port", usedLocalPort,
					"uptime", time.Since(startTime).Round(time.Second),
				)
				_ = conn.Close()
			})

			slog.Debug("ssh-proxy connection established", "localAddr", conn.LocalAddr(), "remoteAddr", conn.RemoteAddr())

			// Close conn on context cancellation (SIGINT/SIGTERM) to unblock io.Copy goroutines
			go func() {
				<-cmd.Context().Done()
				_ = conn.Close()
			}()

			// Layer 2: Data flow monitor
			var lastData atomic.Int64
			lastData.Store(time.Now().UnixNano())
			watchdogCtx, watchdogCancel := context.WithCancel(cmd.Context())
			defer watchdogCancel()
			go proxyDataFlowWatchdog(watchdogCtx, conn, &lastData, 5*time.Second, 15*time.Second)

			var copyErr error
			var once sync.Once
			done := make(chan struct{})
			setErr := func(err error) {
				if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || isIgnorableProxyIOError(err) {
					return
				}
				slog.Debug("ssh-proxy copy error", "error", err)
				once.Do(func() { copyErr = err })
			}
			go func() {
				slog.Debug("ssh-proxy starting copy: stdin -> conn")
				_, err := monitoredCopy(conn, os.Stdin, &lastData)
				setErr(err)
				slog.Debug("ssh-proxy finished copy: stdin -> conn")
				select {
				case <-done:
				default:
					_ = conn.Close()
				}
			}()
			go func() {
				slog.Debug("ssh-proxy starting copy: conn -> stdout")
				_, err := monitoredCopy(os.Stdout, conn, &lastData)
				setErr(err)
				slog.Debug("ssh-proxy finished copy: conn -> stdout")
				close(done)
				_ = conn.Close()
			}()
			<-done
			watchdogCancel()
			slog.Info("ssh-proxy session finished",
				"error", copyErr,
				"uptime", time.Since(startTime).Round(time.Second),
			)
			if copyErr != nil {
				return fmt.Errorf("%w: %v", ErrProxyHealthDisconnect, copyErr)
			}
			return nil
```

Note: Remove the `var wg sync.WaitGroup` and `wg.Add(2)` / `defer wg.Done()` — they were unused (the function waits on `<-done`, not `wg.Wait()`).

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/acmore/workspace/okdev && go build ./...`
Expected: success

- [ ] **Step 3: Run existing tests to check no regressions**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -v -count=1`
Expected: PASS

- [ ] **Step 4: Commit**

```bash
git add internal/cli/ssh.go
git commit -m "feat: wire three-layer health monitoring into ssh-proxy command"
```

### Task 4: Suppress sentinel error in main.go

**Files:**
- Modify: `cmd/okdev/main.go`

- [ ] **Step 1: Update main.go to suppress proxy sentinel error**

```go
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/acmore/okdev/internal/cli"
)

func main() {
	if err := cli.Execute(); err != nil {
		if !errors.Is(err, cli.ErrProxyHealthDisconnect) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(1)
	}
}
```

- [ ] **Step 2: Verify it compiles**

Run: `cd /Users/acmore/workspace/okdev && go build ./cmd/okdev/`
Expected: success

- [ ] **Step 3: Commit**

```bash
git add cmd/okdev/main.go
git commit -m "feat: suppress proxy health disconnect sentinel from stderr"
```

---

## Chunk 3: SSH Config Keepalive Hardcoding and Tests

### Task 5: Hardcode SSH config keepalive values for proxy path

**Files:**
- Modify: `internal/cli/ssh.go:441-444`

- [ ] **Step 1: Write the failing test**

Add to `internal/cli/ssh_config_test.go`:

```go
func TestEnsureSSHConfigEntryUsesProxyKeepaliveValues(t *testing.T) {
	home := t.TempDir()
	origHome := os.Getenv("HOME")
	if err := os.Setenv("HOME", home); err != nil {
		t.Fatalf("set HOME: %v", err)
	}
	defer func() {
		_ = os.Setenv("HOME", origHome)
	}()

	// Pass config defaults (10/30) — the written config should still use 5/10
	_, err := ensureSSHConfigEntry(
		"okdev-test2",
		"test-session2",
		"dev-ns",
		"root",
		22,
		"/tmp/id_ed25519",
		"/tmp/.okdev.yaml",
		nil,
		config.SSHSpec{KeepAliveInterval: 10, KeepAliveCountMax: 30},
	)
	if err != nil {
		t.Fatalf("ensureSSHConfigEntry: %v", err)
	}

	cfgPath := filepath.Join(home, ".ssh", "config")
	b, err := os.ReadFile(cfgPath)
	if err != nil {
		t.Fatalf("read ssh config: %v", err)
	}
	text := string(b)
	if !strings.Contains(text, "ServerAliveInterval 5") {
		t.Fatalf("expected ServerAliveInterval 5, got: %s", text)
	}
	if !strings.Contains(text, "ServerAliveCountMax 10") {
		t.Fatalf("expected ServerAliveCountMax 10, got: %s", text)
	}
}
```

- [ ] **Step 2: Run test to verify it fails**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -run TestEnsureSSHConfigEntryUsesProxyKeepaliveValues -v`
Expected: FAIL — will contain `ServerAliveInterval 10` and `ServerAliveCountMax 30`

- [ ] **Step 3: Hardcode the keepalive values in `ensureSSHConfigEntry`**

In `internal/cli/ssh.go` lines 441-443, replace:

```go
	blockLines = append(blockLines,
		fmt.Sprintf("  ServerAliveInterval %d", sshSpec.KeepAliveInterval),
		fmt.Sprintf("  ServerAliveCountMax %d", sshSpec.KeepAliveCountMax),
```

With:

```go
	// Hardcoded keepalive values for the proxy path — sshSpec values are
	// intentionally ignored here because the proxy needs tighter detection
	// than the general SSH config defaults provide.
	blockLines = append(blockLines,
		"  ServerAliveInterval 5",
		"  ServerAliveCountMax 10",
```

- [ ] **Step 4: Run tests to verify pass**

Run: `cd /Users/acmore/workspace/okdev && go test ./internal/cli/ -run 'TestEnsureSSHConfigEntry' -v`
Expected: PASS (both tests)

- [ ] **Step 5: Run full test suite**

Run: `cd /Users/acmore/workspace/okdev && go test ./... -count=1`
Expected: PASS

- [ ] **Step 6: Commit**

```bash
git add internal/cli/ssh.go internal/cli/ssh_config_test.go
git commit -m "feat: hardcode ServerAliveInterval 5 / ServerAliveCountMax 10 in SSH config"
```
