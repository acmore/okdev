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

type ConnectionState string

const (
	StateDisconnected ConnectionState = "disconnected"
	StateReconnecting ConnectionState = "reconnecting"
	StateConnected    ConnectionState = "connected"
)

var ErrLocalPortInUse = errors.New("local port already in use")

// TunnelManager manages one SSH client connection and a set of local forwards.
type TunnelManager struct {
	SSHUser           string
	SSHKeyPath        string
	RemotePort        int
	Env               map[string]string
	KeepAliveInterval time.Duration
	KeepAliveTimeout  time.Duration

	mu        sync.Mutex
	client    *xssh.Client
	listeners map[int]net.Listener
	ctx       context.Context
	cancel    context.CancelFunc

	autoCancel context.CancelFunc

	lastHost string
	lastPort int

	keepAliveOnce     sync.Once
	reconnecting      bool
	stateCallback     func(ConnectionState)
	reconnectTargetFn func(context.Context) (string, int, error)
	rttCallback       func(time.Duration)
}

// Connect establishes an SSH connection to host:port.
func (tm *TunnelManager) Connect(ctx context.Context, host string, port int) error {
	client, err := tm.dialSSH(host, port)
	if err != nil {
		return err
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.ctx == nil {
		tm.ctx, tm.cancel = context.WithCancel(ctx)
	}
	if tm.listeners == nil {
		tm.listeners = map[int]net.Listener{}
	}
	tm.closeClientLocked()
	tm.client = client
	tm.lastHost = host
	tm.lastPort = port
	tm.reconnecting = false
	tm.keepAliveOnce.Do(func() { go tm.keepAliveLoop() })
	tm.emitStateLocked(StateConnected)
	return nil
}

func (tm *TunnelManager) dialSSH(host string, port int) (*xssh.Client, error) {
	keyBytes, err := os.ReadFile(tm.SSHKeyPath)
	if err != nil {
		return nil, fmt.Errorf("read ssh key: %w", err)
	}
	signer, err := xssh.ParsePrivateKey(keyBytes)
	if err != nil {
		return nil, fmt.Errorf("parse ssh key: %w", err)
	}
	cfg := &xssh.ClientConfig{
		User:            tm.SSHUser,
		Auth:            []xssh.AuthMethod{xssh.PublicKeys(signer)},
		HostKeyCallback: xssh.InsecureIgnoreHostKey(),
		Timeout:         10 * time.Second,
	}
	client, err := xssh.Dial("tcp", fmt.Sprintf("%s:%d", host, port), cfg)
	if err != nil {
		return nil, fmt.Errorf("ssh dial: %w", err)
	}
	return client, nil
}

func (tm *TunnelManager) SetConnectionStateCallback(cb func(ConnectionState)) {
	tm.mu.Lock()
	tm.stateCallback = cb
	tm.mu.Unlock()
}

func (tm *TunnelManager) SetReconnectTargetProvider(fn func(context.Context) (string, int, error)) {
	tm.mu.Lock()
	tm.reconnectTargetFn = fn
	tm.mu.Unlock()
}

func (tm *TunnelManager) SetKeepAliveRTTCallback(cb func(time.Duration)) {
	tm.mu.Lock()
	tm.rttCallback = cb
	tm.mu.Unlock()
}

func (tm *TunnelManager) emitStateLocked(state ConnectionState) {
	if tm.stateCallback != nil {
		go tm.stateCallback(state)
	}
}

// Close tears down all listeners and the SSH connection.
func (tm *TunnelManager) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	return tm.closeLocked()
}

// ListListeningPorts queries remote TCP listeners.
func (tm *TunnelManager) ListListeningPorts() ([]int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	out, err := tm.ExecContext(ctx, "ss -H -ltn")
	if err != nil {
		// Fallback for minimal images without `ss`.
		out, err = tm.ExecContext(ctx, "cat /proc/net/tcp /proc/net/tcp6")
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
	ctx := tm.ctx
	if tm.client == nil || ctx == nil {
		tm.mu.Unlock()
		return fmt.Errorf("not connected")
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		if isAddrInUse(err) {
			tm.mu.Unlock()
			return fmt.Errorf("%w: %v", ErrLocalPortInUse, err)
		}
		tm.mu.Unlock()
		return fmt.Errorf("listen local port %d: %w", localPort, err)
	}
	tm.listeners[localPort] = ln
	tm.mu.Unlock()

	go tm.serveForward(ctx, ln, remotePort)
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
	return tm.ExecContext(context.Background(), cmd)
}

func (tm *TunnelManager) ExecContext(ctx context.Context, cmd string) ([]byte, error) {
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
	type execResult struct {
		out []byte
		err error
	}
	done := make(chan execResult, 1)
	go func() {
		out, err := s.CombinedOutput(cmd)
		done <- execResult{out: out, err: err}
	}()
	select {
	case <-ctx.Done():
		_ = s.Close()
		return nil, ctx.Err()
	case r := <-done:
		if r.err != nil {
			return r.out, fmt.Errorf("exec command: %w", r.err)
		}
		return r.out, nil
	}
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

	for k, v := range tm.Env {
		_ = s.Setenv(k, v)
	}

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

func (tm *TunnelManager) keepAliveLoop() {
	interval := tm.KeepAliveInterval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	timeout := tm.KeepAliveTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	if timeout < interval {
		timeout = interval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-tm.contextDone():
			return
		case <-ticker.C:
			tm.mu.Lock()
			client := tm.client
			connected := client != nil
			rttCB := tm.rttCallback
			tm.mu.Unlock()
			if !connected {
				continue
			}
			rtt, err := tm.sendKeepAlive(client, timeout)
			if err != nil {
				tm.handleConnectionLoss(client)
				continue
			}
			if rttCB != nil {
				go rttCB(rtt)
			}
		}
	}
}

func (tm *TunnelManager) sendKeepAlive(client *xssh.Client, timeout time.Duration) (time.Duration, error) {
	start := time.Now()
	done := make(chan error, 1)
	go func() {
		_, _, err := client.SendRequest("keepalive@openssh.com", true, nil)
		done <- err
	}()
	select {
	case err := <-done:
		return time.Since(start), err
	case <-time.After(timeout):
		return 0, fmt.Errorf("keepalive timeout")
	case <-tm.contextDone():
		return 0, context.Canceled
	}
}

func (tm *TunnelManager) serveForward(ctx context.Context, ln net.Listener, remotePort int) {
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
			tm.mu.Lock()
			client := tm.client
			tm.mu.Unlock()
			if client == nil {
				return
			}
			remoteConn, err := client.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
			if err != nil {
				return
			}
			defer remoteConn.Close()
			copyBothWays(ctx, localConn, remoteConn)
		}(localConn)
	}
}

func (tm *TunnelManager) contextDone() <-chan struct{} {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.ctx == nil {
		ch := make(chan struct{})
		close(ch)
		return ch
	}
	return tm.ctx.Done()
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
	tm.closeClientLocked()
	tm.emitStateLocked(StateDisconnected)
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
	err := tm.closeClientLocked()
	if closeErr == nil {
		closeErr = err
	}
	tm.ctx = nil
	tm.reconnecting = false
	tm.emitStateLocked(StateDisconnected)
	return closeErr
}

func (tm *TunnelManager) closeClientLocked() error {
	var closeErr error
	if tm.client != nil {
		if err := tm.client.Close(); err != nil && !errors.Is(err, net.ErrClosed) && closeErr == nil {
			closeErr = err
		}
		tm.client = nil
	}
	return closeErr
}

func (tm *TunnelManager) handleConnectionLoss(client *xssh.Client) {
	tm.mu.Lock()
	if tm.client != client || tm.reconnecting {
		tm.mu.Unlock()
		return
	}
	tm.reconnecting = true
	tm.closeClientLocked()
	tm.emitStateLocked(StateReconnecting)
	tm.mu.Unlock()

	ctx := tm.managerContext()
	err := reconnectWithBackoff(ctx, func() error {
		host, port, err := tm.resolveReconnectTarget(ctx)
		if err != nil {
			return err
		}
		nextClient, err := tm.dialSSH(host, port)
		if err != nil {
			return err
		}
		tm.mu.Lock()
		tm.client = nextClient
		tm.lastHost = host
		tm.lastPort = port
		tm.reconnecting = false
		tm.emitStateLocked(StateConnected)
		tm.mu.Unlock()
		return nil
	}, 0, 1*time.Second, 30*time.Second)
	if err != nil {
		tm.mu.Lock()
		tm.reconnecting = false
		tm.emitStateLocked(StateDisconnected)
		tm.mu.Unlock()
	}
}

func (tm *TunnelManager) managerContext() context.Context {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.ctx == nil {
		return context.Background()
	}
	return tm.ctx
}

func (tm *TunnelManager) resolveReconnectTarget(ctx context.Context) (string, int, error) {
	tm.mu.Lock()
	fn := tm.reconnectTargetFn
	host := tm.lastHost
	port := tm.lastPort
	tm.mu.Unlock()
	if fn == nil {
		if host == "" || port <= 0 {
			return "", 0, fmt.Errorf("reconnect target unavailable")
		}
		return host, port, nil
	}
	return fn(ctx)
}

func reconnectWithBackoff(ctx context.Context, connectFn func() error, maxRetries int, initialBackoff, maxBackoff time.Duration) error {
	if initialBackoff <= 0 {
		initialBackoff = time.Second
	}
	if maxBackoff < initialBackoff {
		maxBackoff = initialBackoff
	}
	backoff := initialBackoff
	for attempt := 0; maxRetries == 0 || attempt < maxRetries; attempt++ {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		if err := connectFn(); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
	return fmt.Errorf("reconnect failed after %d attempts", maxRetries)
}

func isAddrInUse(err error) bool {
	var opErr *net.OpError
	if errors.As(err, &opErr) {
		var sysErr *os.SyscallError
		if errors.As(opErr.Err, &sysErr) {
			return errors.Is(sysErr.Err, syscall.EADDRINUSE)
		}
	}
	return strings.Contains(strings.ToLower(err.Error()), "address already in use")
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
