package ssh

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"sync"
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
	if tm.client != nil {
		_ = tm.client.Close()
	}
	tm.client = client
	tm.ctx, tm.cancel = context.WithCancel(ctx)
	if tm.listeners == nil {
		tm.listeners = map[int]net.Listener{}
	}
	return nil
}

// Close tears down all listeners and the SSH connection.
func (tm *TunnelManager) Close() error {
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if tm.cancel != nil {
		tm.cancel()
	}
	for port, ln := range tm.listeners {
		_ = ln.Close()
		delete(tm.listeners, port)
	}
	if tm.client == nil {
		return nil
	}
	err := tm.client.Close()
	tm.client = nil
	return err
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
	client := tm.client
	if tm.listeners == nil {
		tm.listeners = map[int]net.Listener{}
	}
	if _, exists := tm.listeners[localPort]; exists {
		tm.mu.Unlock()
		return nil
	}
	tm.mu.Unlock()

	if client == nil {
		return fmt.Errorf("not connected")
	}

	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", localPort))
	if err != nil {
		return fmt.Errorf("listen local port %d: %w", localPort, err)
	}

	tm.mu.Lock()
	tm.listeners[localPort] = ln
	tm.mu.Unlock()

	go tm.serveForward(ln, client, remotePort)
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
	}

	if err := s.Shell(); err != nil {
		return fmt.Errorf("start shell: %w", err)
	}
	if err := s.Wait(); err != nil {
		return fmt.Errorf("wait shell: %w", err)
	}
	return nil
}

func (tm *TunnelManager) serveForward(ln net.Listener, client *xssh.Client, remotePort int) {
	for {
		localConn, err := ln.Accept()
		if err != nil {
			return
		}
		go func() {
			defer localConn.Close()
			remoteConn, err := client.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", remotePort))
			if err != nil {
				return
			}
			defer remoteConn.Close()
			copyBothWays(localConn, remoteConn)
		}()
	}
}

func copyBothWays(a net.Conn, b net.Conn) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		_, _ = io.Copy(a, b)
	}()
	go func() {
		defer wg.Done()
		_, _ = io.Copy(b, a)
	}()
	wg.Wait()
}
