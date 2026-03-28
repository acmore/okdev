package ssh

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	gssh "github.com/gliderlabs/ssh"
	xssh "golang.org/x/crypto/ssh"
)

func TestTunnelManagerConnectExecAndListPorts(t *testing.T) {
	keyPath, pubKey := writeSSHKeyPair(t)
	srv, addr := startTestSSHServer(t, pubKey)

	tm := &TunnelManager{
		SSHUser:    "tester",
		SSHKeyPath: keyPath,
		RemotePort: addr.Port,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := tm.Connect(ctx, addr.IP.String(), addr.Port); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		_ = tm.Close()
		_ = srv.Close()
	}()

	out, err := tm.Exec("printf 'hello'")
	if err != nil {
		t.Fatalf("Exec: %v", err)
	}
	if string(out) != "hello" {
		t.Fatalf("unexpected exec output %q", string(out))
	}

	ctxTimeout, cancelTimeout := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancelTimeout()
	if _, err := tm.ExecContext(ctxTimeout, "sleep 1"); err == nil {
		t.Fatal("expected ExecContext timeout")
	}
}

func writeSSHKeyPair(t *testing.T) (string, xssh.PublicKey) {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate key: %v", err)
	}
	privDER := x509.MarshalPKCS1PrivateKey(priv)
	pemBytes := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: privDER})
	keyPath := filepath.Join(t.TempDir(), "id_rsa")
	if err := os.WriteFile(keyPath, pemBytes, 0o600); err != nil {
		t.Fatalf("write private key: %v", err)
	}
	signer, err := xssh.NewSignerFromKey(priv)
	if err != nil {
		t.Fatalf("new signer: %v", err)
	}
	return keyPath, signer.PublicKey()
}

func startTestSSHServer(t *testing.T, allowedKey xssh.PublicKey) (*gssh.Server, *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	srv := &gssh.Server{
		Addr: fmt.Sprintf("127.0.0.1:%d", addr.Port),
		Handler: func(s gssh.Session) {
			cmd := exec.Command("/bin/sh", "-lc", s.RawCommand())
			cmd.Stdin = s
			cmd.Stdout = s
			cmd.Stderr = s.Stderr()
			if err := cmd.Run(); err != nil {
				_ = s.Exit(1)
				return
			}
			_ = s.Exit(0)
		},
		PublicKeyHandler: func(ctx gssh.Context, key gssh.PublicKey) bool {
			return bytes.Equal(key.Marshal(), allowedKey.Marshal())
		},
	}

	go func() {
		_ = srv.ListenAndServe()
	}()

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return srv, addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for ssh server")
	return nil, nil
}

func TestTunnelManagerListListeningPortsFallbackToProcNet(t *testing.T) {
	keyPath, pubKey := writeSSHKeyPair(t)
	srv, addr := startTestSSHServerWithHandler(t, pubKey, func(s gssh.Session) {
		switch {
		case strings.Contains(s.RawCommand(), "ss -H -ltn"):
			io.WriteString(s.Stderr(), "ss missing\n")
			_ = s.Exit(1)
		case strings.Contains(s.RawCommand(), "cat /proc/net/tcp /proc/net/tcp6"):
			io.WriteString(s, "  sl  local_address rem_address   st\n   0: 0100007F:1F90 00000000:0000 0A\n")
			_ = s.Exit(0)
		default:
			_ = s.Exit(1)
		}
	})

	tm := &TunnelManager{
		SSHUser:    "tester",
		SSHKeyPath: keyPath,
		RemotePort: addr.Port,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := tm.Connect(ctx, addr.IP.String(), addr.Port); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	defer func() {
		_ = tm.Close()
		_ = srv.Close()
	}()

	ports, err := tm.ListListeningPorts()
	if err != nil {
		t.Fatalf("ListListeningPorts: %v", err)
	}
	if len(ports) != 1 || ports[0] != 8080 {
		t.Fatalf("unexpected ports %v", ports)
	}
}

func startTestSSHServerWithHandler(t *testing.T, allowedKey xssh.PublicKey, handler func(gssh.Session)) (*gssh.Server, *net.TCPAddr) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().(*net.TCPAddr)
	_ = ln.Close()

	srv := &gssh.Server{
		Addr:    fmt.Sprintf("127.0.0.1:%d", addr.Port),
		Handler: handler,
		PublicKeyHandler: func(ctx gssh.Context, key gssh.PublicKey) bool {
			return bytes.Equal(key.Marshal(), allowedKey.Marshal())
		},
	}
	go func() {
		_ = srv.ListenAndServe()
	}()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr.String(), 100*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return srv, addr
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for ssh server")
	return nil, nil
}
