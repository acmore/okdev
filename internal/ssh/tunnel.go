package ssh

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	xssh "golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

// Forward describes one local->remote tunnel mapping over SSH.
type Forward struct {
	LocalPort  int
	RemotePort int
	Auto       bool
}

// TunnelManager manages one SSH client connection and a set of local forwards.
type TunnelManager struct {
	SSHUser    string
	SSHKeyPath string
	RemotePort int

	mu        sync.Mutex
	client    *xssh.Client
	listeners map[int]net.Listener
	ctx       context.Context
	cancel    context.CancelFunc

	autoCancel context.CancelFunc
}

// Connect establishes an SSH connection to host:port.
func (tm *TunnelManager) Connect(ctx context.Context, host string, port int) error {
	keyBytes, err := os.ReadFile(tm.SSHKeyPath)
	if err != nil {
		return fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := xssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return fmt.Errorf("parse ssh key: %w", err)
	}
	cfg := &xssh.ClientConfig{
		User:            tm.SSHUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := xssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), cfg)
	if err != nil {
		return fmt.Errorf("ssh dial: %w", err)
	}

	tm.mu.Lock()
	defer tm.mu.Unlock()
	tm.closeLocked()
	tm.client = client
	tm.ctx, tm.cancel = context.WithCancel(ctx)
	if tm.listeners == nil {
		tm.listeners = map[int]net.Listener{}
	}

	go tm.keepAlive(tm.ctx, client)

	return nil
}

// Close tears down all listeners and the SSH connection.
func (tm *TunnelManager) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.closeLocked()
}

// ListListeningPorts queries remote TCP listeners.
func (tm *TunnelManager) ListListeningPorts() ([]int, error) {
	out, err := tm.Exec("ss -H -ltn")
	if err != nil {
		// Fallback for minimal images without `ss`.
		out, err = tm.Exec("cat /proc/net/tcp /proc/net/tcp6")
		if err != nil {
			return nil, err
		}
		return parseProcNetTCPPorts(string(out)), nil
	}
	return parseSSListeningPorts(string(out)), nil
}

// StartAutoDetect polls listening ports and calls onAdd/onRemove for diffs.
func (tm *TunnelManager) StartAutoDetect(interval time.Duration, configured map[int]struct{}, exclude map[int]struct{}, onAdd func(port int), onRemove func(port int)) {
	tm.mu.Lock()
	if tm.autoCancel != nil {
		tm.autoCancel()
	}
	ctx := tm.ctx
	if ctx == nil {
		ctx = context.Background()
	}
	autoCtx, cancel := context.WithCancel(ctx)
	tm.autoCancel = cancel
	tm.mu.Unlock()

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		last := map[int]struct{}{}
		for {
			ports, err := tm.ListListeningPorts()
			if err == nil {
				curr := map[int]struct{}{}
				for _, p := range ports {
					if _, ok := configured[p]; ok {
						continue
					}
					if _, ok := exclude[p]; ok {
						continue
					}
					curr[p] = struct{}{}
				}
				for p := range curr {
					if _, existed := last[p]; !existed && onAdd != nil {
						onAdd(p)
					}
				}
				for p := range last {
					if _, still := curr[p]; !still && onRemove != nil {
						onRemove(p)
					}
				}
				last = curr
			}

			select {
			case <-autoCtx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
}

// IsConnected reports if an SSH client is present.
func (tm *TunnelManager) IsConnected() bool {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.client != nil
}

// AddForward starts a local listener and forwards accepted connections to remotePort.
func (tm *TunnelManager) AddForward(localPort, remotePort int) error {
	tm.mu.Lock()
	if tm.listeners == nil {
		tm.listeners = map[int]net.Listener{}
	}
	if _, exists := tm.listeners[localPort]; exists {
		tm.mu.Unlock()
		return nil
	}
	client := tm.client
	ctx := tm.ctx
	if client == nil || ctx == nil {
		tm.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		tm.mu.Unlock()
		return fmt.Errorf("listen local port %d: %w", localPort, err)
	}
	tm.listeners[localPort] = ln
	tm.mu.Unlock()

	go tm.serveForward(ctx, ln, client, remotePort)
	return nil
}

// RemoveForward stops a local listener for localPort.
func (tm *TunnelManager) RemoveForward(localPort int) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if ln, ok := tm.listeners[localPort]; ok {
		_ = ln.Close()
		delete(tm.listeners, localPort)
	}
}

// Exec runs a remote command on the connected SSH client.
func (tm *TunnelManager) Exec(cmd string) ([]byte, error) {
	tm.mu.Lock()
	client := tm.client
	tm.mu.Unlock()
	if client == nil {
		return nil, fmt.Errorf("not connected")
	}
	s, err := client.NewSession()
	if err != nil {
		return nil, fmt.Errorf("new ssh session: %w", err)
	}
	defer s.Close()
	out, err := s.CombinedOutput(cmd)
	if err != nil {
		return out, fmt.Errorf("exec command: %w", err)
	}
	return out, nil
}

// OpenShell opens an interactive shell over the current SSH client.
func (tm *TunnelManager) OpenShell() error {
	tm.mu.Lock()
	client := tm.client
	tm.mu.Unlock()
	if client == nil {
		return fmt.Errorf("not connected")
	}
	s, err := client.NewSession()
	if err != nil {
		return fmt.Errorf("new ssh session: %w", err)
	}
	defer s.Close()

	s.Stdin = os.Stdin
	s.Stdout = os.Stdout
	s.Stderr = os.Stderr

	fd := int(os.Stdin.Fd())
	if term.IsTerminal(fd) {
		oldState, err := term.MakeRaw(fd)
		if err != nil {
			return fmt.Errorf("set terminal raw mode: %w", err)
		}
		defer func() {
			_ = term.Restore(fd, oldState)
		}()

		w, h, err := term.GetSize(fd)
		if err == nil {
			if err := s.RequestPty("xterm-256color", h, w, xssh.TerminalModes{
				xssh.ECHO:          1,
				xssh.TTY_OP_ISPEED: 14400,
				xssh.TTY_OP_OSPEED: 14400,
			}); err != nil {
				return fmt.Errorf("request pty: %w", err)
			}
		}
		winch := make(chan os.Signal, 1)
		done := make(chan struct{})
		defer close(done)
		signal.Notify(winch, syscall.SIGWINCH)
		defer signal.Stop(winch)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-winch:
					w, h, err := term.GetSize(fd)
					if err == nil {
						_ = s.WindowChange(h, w)
					}
				}
			}
		}()
	}

	if err := s.Shell(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}
	if err := s.Wait(); err != nil {
		return fmt.Errorf("wait shell: %w", err)
	}
	return nil
}

func (tm *TunnelManager) keepAlive(ctx context.Context, client *xssh.Client) {
	ticker := time.NewTicker(10 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			done := make(chan error, 1)
			go func() {
				_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
				done <- err
			}()
			select {
			case err := <-done:
				if err != nil {
					tm.disconnectClient(client)
					return
				}
			case <-time.After(15 * time.Second):
				tm.disconnectClient(client)
				return
			case <-ctx.Done():
				return
			}
		}
	}
}

func (tm *TunnelManager) serveForward(ctx context.Context, ln net.Listener, client *xssh.Client, remotePort int) {
	go func() {
		<-ctx.Done()
		_ = ln.Close()
	}()
	for {
		localConn, err := ln.Accept()
		if err != nil {
			return
		}
		go func(localConn net.Conn) {
			defer localConn.Close()
			remoteConn, err := client.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
			if err != nil {
				return
			}
			defer remoteConn.Close()
			copyBothWays(ctx, localConn, remoteConn)
		}(localConn)
	}
}

func copyBothWays(ctx context.Context, a net.Conn, b net.Conn) {
	go func() {
		<-ctx.Done()
		_ = a.Close()
		_ = b.Close()
	}()
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
		closeWrite(a)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
		closeWrite(b)
	}()
	wg.Wait()
}

func closeWrite(conn net.Conn) {
	if cw, ok := conn.(interface{ CloseWrite() error }); ok {
		_ = cw.CloseWrite()
	}
}

func parseSSListeningPorts(raw string) []int {
	ports := map[int]struct{}{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		localAddr := fields[3]
		port, ok := parsePortFromAddr(localAddr)
		if ok {
			ports[port] = struct{}{}
		}
	}
	return sortedPorts(ports)
}

func parseProcNetTCPPorts(raw string) []int {
	ports := map[int]struct{}{}
	sc := bufio.NewScanner(strings.NewReader(raw))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "sl") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 4 {
			continue
		}
		if !strings.EqualFold(fields[3], "0A") { // LISTEN
			continue
		}
		local := fields[1]
		parts := strings.Split(local, ":")
		if len(parts) != 2 {
			continue
		}
		p, err := strconv.ParseInt(parts[1], 16, 32)
		if err != nil {
			continue
		}
		if p > 0 {
			ports[int(p)] = struct{}{}
		}
	}
	return sortedPorts(ports)
}

func (tm *TunnelManager) disconnectClient(client *xssh.Client) {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.client != client {
		return
	}
	_ = tm.closeLocked()
}

func (tm *TunnelManager) closeLocked() error {
	if tm.autoCancel != nil {
		tm.autoCancel()
		tm.autoCancel = nil
	}
	if tm.cancel != nil {
		tm.cancel()
		tm.cancel = nil
	}
	var closeErr error
	for port, ln := range tm.listeners {
		if err := ln.Close(); err != nil && !errors.Is(err, net.ErrClosed) && closeErr == nil {
			closeErr = err
		}
		delete(tm.listeners, port)
	}
	if tm.client != nil {
		if err := tm.client.Close(); err != nil && !errors.Is(err, net.ErrClosed) && closeErr == nil {
			closeErr = err
		}
		tm.client = nil
	}
	tm.ctx = nil
	return closeErr
}

func parsePortFromAddr(addr string) (int, bool) {
	i := strings.LastIndex(addr, ":")
	if i < 0 || i == len(addr)-1 {
		return 0, false
	}
	p, err := strconv.Atoi(addr[i+1:])
	if err != nil || p <= 0 {
		return 0, false
	}
	return p, true
}

func sortedPorts(m map[int]struct{}) []int {
	out := make([]int, 0, len(m))
	for p := range m {
		out = append(out, p)
	}
	// Small sizes expected; keep dependency surface low.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j] < out[i] {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	return out
}
