package ssh

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"sort"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

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

type closeWriteConn struct {
	net.Conn
	called bool
}

func (c *closeWriteConn) CloseWrite() error {
	c.called = true
	return nil
}

func TestTunnelManagerCallbacksAndWaitConnected(t *testing.T) {
	tm := &TunnelManager{}
	stateCh := make(chan ConnectionState, 1)
	rttCh := make(chan time.Duration, 1)
	providerCalled := make(chan struct{}, 1)

	tm.SetConnectionStateCallback(func(state ConnectionState) { stateCh <- state })
	tm.SetKeepAliveRTTCallback(func(rtt time.Duration) { rttCh <- rtt })
	tm.SetReconnectTargetProvider(func(context.Context) (string, int, error) {
		providerCalled <- struct{}{}
		return "127.0.0.1", 2222, nil
	})

	waitCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	done := make(chan bool, 1)
	go func() {
		done <- tm.WaitConnected(waitCtx)
	}()

	tm.mu.Lock()
	tm.client = &xssh.Client{}
	tm.emitStateLocked(StateConnected)
	cb := tm.stateCallback
	rtt := tm.rttCallback
	tm.mu.Unlock()

	if !<-done {
		t.Fatal("expected WaitConnected to unblock when connected")
	}
	select {
	case state := <-stateCh:
		if state != StateConnected {
			t.Fatalf("unexpected state %q", state)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for state callback")
	}

	cb(StateConnected)
	rtt(25 * time.Millisecond)
	select {
	case <-rttCh:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for rtt callback")
	}

	if host, port, err := tm.resolveReconnectTarget(context.Background()); err != nil || host != "127.0.0.1" || port != 2222 {
		t.Fatalf("unexpected reconnect target: host=%q port=%d err=%v", host, port, err)
	}
	select {
	case <-providerCalled:
	case <-time.After(time.Second):
		t.Fatal("expected reconnect target provider to be used")
	}
}

func TestTunnelManagerResolveReconnectTargetFallback(t *testing.T) {
	tm := &TunnelManager{lastHost: "example.com", lastPort: 2200}
	host, port, err := tm.resolveReconnectTarget(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if host != "example.com" || port != 2200 {
		t.Fatalf("unexpected fallback target: %s:%d", host, port)
	}
}

func TestTunnelManagerResolveReconnectTargetUnavailable(t *testing.T) {
	tm := &TunnelManager{}
	if _, _, err := tm.resolveReconnectTarget(context.Background()); err == nil {
		t.Fatal("expected unavailable target error")
	}
}

func TestReconnectWithBackoffHonorsContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := reconnectWithBackoff(ctx, func() error {
		t.Fatal("connectFn should not be called when context is already canceled")
		return nil
	}, 3, 5*time.Millisecond, 20*time.Millisecond)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context canceled, got %v", err)
	}
}

func TestTunnelManagerCloseWakesWaitersAndClosesListeners(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen failed: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	tm := &TunnelManager{
		ctx:       ctx,
		cancel:    cancel,
		listeners: map[int]net.Listener{port: ln},
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
	defer waitCancel()
	done := make(chan bool, 1)
	go func() {
		done <- tm.WaitConnected(waitCtx)
	}()

	if err := tm.Close(); err != nil {
		t.Fatalf("unexpected close error: %v", err)
	}
	if tm.ctx != nil {
		t.Fatal("expected manager context to be cleared")
	}
	if len(tm.listeners) != 0 {
		t.Fatalf("expected listeners to be cleared, got %d", len(tm.listeners))
	}
	if <-done {
		t.Fatal("expected waiter to observe closed tunnel")
	}
}

type errListener struct {
	net.Listener
	closeErr error
	closed   bool
}

func (l *errListener) Close() error {
	l.closed = true
	return l.closeErr
}

func TestTunnelManagerCloseReturnsListenerError(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ln := &errListener{closeErr: errors.New("close failed")}
	tm := &TunnelManager{
		ctx:       ctx,
		cancel:    cancel,
		listeners: map[int]net.Listener{1: ln},
	}
	err := tm.Close()
	if err == nil || !strings.Contains(err.Error(), "close failed") {
		t.Fatalf("expected listener close error, got %v", err)
	}
	if !ln.closed {
		t.Fatal("expected listener to be closed")
	}
}

func TestTunnelManagerManagerContextDefaultsToBackground(t *testing.T) {
	tm := &TunnelManager{}
	select {
	case <-tm.managerContext().Done():
		t.Fatal("background context should not be done")
	default:
	}
}

func TestContextDoneClosedWithoutManagerContext(t *testing.T) {
	tm := &TunnelManager{}
	select {
	case <-tm.contextDone():
	default:
		t.Fatal("expected closed channel when no manager context exists")
	}
}

func TestContextDoneUsesManagerContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	tm := &TunnelManager{ctx: ctx}
	cancel()
	select {
	case <-tm.contextDone():
	case <-time.After(time.Second):
		t.Fatal("expected manager context done channel to close")
	}
}

func TestTunnelManagerIsConnected(t *testing.T) {
	tm := &TunnelManager{}
	if tm.IsConnected() {
		t.Fatal("expected disconnected manager")
	}
	tm.client = &xssh.Client{}
	if !tm.IsConnected() {
		t.Fatal("expected connected manager")
	}
}

func TestTunnelManagerWaitConnectedReturnsImmediatelyWhenConnected(t *testing.T) {
	tm := &TunnelManager{client: &xssh.Client{}}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if !tm.WaitConnected(ctx) {
		t.Fatal("expected immediate success for connected manager")
	}
}

func TestTunnelManagerWaitConnectedRemovesCanceledWaiter(t *testing.T) {
	tm := &TunnelManager{}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- tm.WaitConnected(ctx)
	}()

	for deadline := time.Now().Add(time.Second); time.Now().Before(deadline); {
		tm.mu.Lock()
		waiters := len(tm.connectedWaiters)
		tm.mu.Unlock()
		if waiters == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	if got := <-done; got {
		t.Fatal("expected canceled wait to return false")
	}
	tm.mu.Lock()
	defer tm.mu.Unlock()
	if len(tm.connectedWaiters) != 0 {
		t.Fatalf("expected canceled waiter to be removed, got %d", len(tm.connectedWaiters))
	}
}

func TestCloseWriteUsesOptionalInterface(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	conn := &closeWriteConn{Conn: left}
	closeWrite(conn)
	if !conn.called {
		t.Fatal("expected CloseWrite to be called when available")
	}
}

func TestCloseWriteNoopWithoutInterface(t *testing.T) {
	left, right := net.Pipe()
	defer left.Close()
	defer right.Close()
	closeWrite(left)
}

func TestTunnelManagerForceReconnectWithoutClientIsNoop(t *testing.T) {
	tm := &TunnelManager{}
	tm.ForceReconnect()
	if tm.reconnecting {
		t.Fatal("did not expect reconnect state without a client")
	}
}

func TestTunnelManagerDisconnectClientIgnoresMismatchedClient(t *testing.T) {
	current := &xssh.Client{}
	other := &xssh.Client{}
	tm := &TunnelManager{client: current}
	tm.disconnectClient(other)
	if tm.client != current {
		t.Fatal("expected current client to remain unchanged")
	}
}

func TestCopyBothWaysCopiesTraffic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	left, leftPeer := net.Pipe()
	right, rightPeer := net.Pipe()
	defer leftPeer.Close()
	defer rightPeer.Close()

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		copyBothWays(ctx, left, right)
	}()

	wantLeft := []byte("from-right")
	wantRight := []byte("from-left")
	gotLeft := make([]byte, len(wantLeft))
	gotRight := make([]byte, len(wantRight))

	readDone := make(chan error, 2)
	go func() {
		_, err := io.ReadFull(leftPeer, gotLeft)
		readDone <- err
	}()
	go func() {
		_, err := io.ReadFull(rightPeer, gotRight)
		readDone <- err
	}()

	if _, err := leftPeer.Write(wantRight); err != nil {
		t.Fatalf("write left peer: %v", err)
	}
	if _, err := rightPeer.Write(wantLeft); err != nil {
		t.Fatalf("write right peer: %v", err)
	}

	for i := 0; i < 2; i++ {
		if err := <-readDone; err != nil {
			t.Fatalf("read forwarded data: %v", err)
		}
	}
	if !reflect.DeepEqual(gotLeft, wantLeft) {
		t.Fatalf("unexpected left payload: got=%q want=%q", gotLeft, wantLeft)
	}
	if !reflect.DeepEqual(gotRight, wantRight) {
		t.Fatalf("unexpected right payload: got=%q want=%q", gotRight, wantRight)
	}

	cancel()
	_ = leftPeer.Close()
	_ = rightPeer.Close()
	wg.Wait()
}

func TestParseSSListeningPortsSkipsInvalidLines(t *testing.T) {
	raw := `
garbage
LISTEN 0 1 missing
LISTEN 0 4096 127.0.0.1:notaport 0.0.0.0:*
`
	if got := parseSSListeningPorts(raw); len(got) != 0 {
		t.Fatalf("expected no parsed ports, got %v", got)
	}
}

func TestSortedPortsEmpty(t *testing.T) {
	if got := sortedPorts(nil); len(got) != 0 {
		t.Fatalf("expected empty result, got %v", got)
	}
}
