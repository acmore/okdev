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

func TestProxyDataFlowWatchdogClosesOnIdle(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

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

	var lastData atomic.Int64
	lastData.Store(time.Now().Add(-20 * time.Second).UnixNano())

	closed := make(chan struct{})
	go func() {
		proxyDataFlowWatchdog(ctx, conn, &lastData, 100*time.Millisecond, 500*time.Millisecond)
		close(closed)
	}()

	select {
	case <-closed:
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

	var lastData atomic.Int64
	lastData.Store(time.Now().UnixNano())

	watchdogDone := make(chan struct{})
	go func() {
		proxyDataFlowWatchdog(ctx, conn, &lastData, 100*time.Millisecond, 500*time.Millisecond)
		close(watchdogDone)
	}()

	time.Sleep(300 * time.Millisecond)
	cancel()

	<-watchdogDone

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
	go func() {
		c, err := ln.Accept()
		if err == nil {
			_ = c.Close()
		}
	}()

	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	setTCPKeepAliveProxyTuning(conn)
}
