package ssh

import (
	"context"
	"errors"
	"net"
	"os"
	"reflect"
	"sort"
	"syscall"
	"testing"

	xssh "golang.org/x/crypto/ssh"
)

func TestTunnelManagerConnectFailsWithBadKeyPath(t *testing.T) {
	tm := &TunnelManager{
		SSHUser:    "root",
		SSHKeyPath: "/definitely/missing/key",
		RemotePort: 2222,
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

func TestTunnelManagerAddForwardReportsLocalPortInUse(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tm := &TunnelManager{
		listeners: map[int]net.Listener{},
		ctx:       ctx,
		client:    &xssh.Client{},
	}
	err = tm.AddForward(port, 8080)
	if err == nil {
		t.Fatal("expected add forward to fail on occupied local port")
	}
	if !errors.Is(err, ErrLocalPortInUse) {
		t.Fatalf("expected ErrLocalPortInUse, got %v", err)
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

func TestIsAddrInUse(t *testing.T) {
	addrInUseErr := &net.OpError{
		Op:  "listen",
		Net: "tcp",
		Err: &os.SyscallError{Syscall: "bind", Err: syscall.EADDRINUSE},
	}
	if !isAddrInUse(addrInUseErr) {
		t.Fatal("expected syscall-based address-in-use detection")
	}
	if !isAddrInUse(errors.New("bind: address already in use")) {
		t.Fatal("expected string-based address-in-use detection")
	}
	if isAddrInUse(errors.New("connection reset by peer")) {
		t.Fatal("did not expect unrelated error to match")
	}
}

func TestParsePortFromAddr(t *testing.T) {
	if port, ok := parsePortFromAddr("127.0.0.1:2222"); !ok || port != 2222 {
		t.Fatalf("unexpected parse result port=%d ok=%v", port, ok)
	}
	if _, ok := parsePortFromAddr("missing-port"); ok {
		t.Fatal("expected parse failure")
	}
	if _, ok := parsePortFromAddr("127.0.0.1:not-a-number"); ok {
		t.Fatal("expected parse failure")
	}
}

func TestSortedPorts(t *testing.T) {
	got := sortedPorts(map[int]struct{}{8080: {}, 22: {}, 22000: {}})
	if !sort.IntsAreSorted(got) {
		t.Fatalf("expected sorted ports, got %v", got)
	}
	want := []int{22, 8080, 22000}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unexpected sorted ports: got=%v want=%v", got, want)
	}
}
