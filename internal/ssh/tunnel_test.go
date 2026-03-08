package ssh

import (
	"context"
	"net"
	"reflect"
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

func TestParseSSListeningPorts(t *testing.T) {
	raw := `
LISTEN 0      4096      127.0.0.1:8080      0.0.0.0:*
LISTEN 0      4096      0.0.0.0:22          0.0.0.0:*
LISTEN 0      4096         [::]:22000          [::]:*
`
	got := parseSSListeningPorts(raw)
	want := []int{22, 8080, 22000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected ports: got=%v want=%v", got, want)
	}
}

func TestParseProcNetTCPPorts(t *testing.T) {
	raw := `
  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000   0        0 0 1 0000000000000000 100 0 0 10 0
   1: 00000000:0016 00000000:0000 0A 00000000:00000000 00:00000000 00000000   0        0 0 1 0000000000000000 100 0 0 10 0
   2: 0100007F:1770 0100007F:8C4A 01 00000000:00000000 00:00000000 00000000   0        0 0 1 0000000000000000 100 0 0 10 0
`
	got := parseProcNetTCPPorts(raw)
	want := []int{22, 8080}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected ports: got=%v want=%v", got, want)
	}
}
