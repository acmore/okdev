package ssh

import (
	"context"
	"net"
	"testing"
)

func TestTunnelManagerConnectFailsWithBadKeyPath(t *testing.T) {
	tm := &TunnelManager{
		SSHUser:    "root",
		SSHKeyPath: "/definitely/missing/key",
		RemotePort: 22,
	}
	if err := tm.Connect(context.Background(), "127.0.0.1", 2222); err == nil {
		t.Fatal("expected connect error with missing key")
	}
}

func TestTunnelManagerAddForwardRequiresConnection(t *testing.T) {
	tm := &TunnelManager{}
	if err := tm.AddForward(18080, 8080); err == nil {
		t.Fatal("expected add forward to fail when disconnected")
	}
}

func TestTunnelManagerRemoveForwardClosesListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	localPort := ln.Addr().(*net.TCPAddr).Port
	tm := &TunnelManager{listeners: map[int]net.Listener{localPort: ln}}
	tm.RemoveForward(localPort)
	if _, ok := tm.listeners[localPort]; ok {
		t.Fatal("listener should be removed")
	}
}
