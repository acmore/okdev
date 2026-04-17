package cli

import (
	"context"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"
)

type blockingPFClient struct {
	done chan struct{}
}

func (b *blockingPFClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	<-ctx.Done()
	close(b.done)
	return ctx.Err()
}

type listeningPFClient struct {
	done chan struct{}
}

func (l *listeningPFClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	if len(forwards) != 1 {
		return nil
	}
	parts := strings.SplitN(forwards[0], ":", 2)
	if len(parts) != 2 {
		return nil
	}
	port, err := strconv.Atoi(parts[0])
	if err != nil {
		return err
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)))
	if err != nil {
		return err
	}
	defer ln.Close()
	<-ctx.Done()
	close(l.done)
	return ctx.Err()
}

func TestStartManagedPortForwardCancelWaitsForWorker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	client := &listeningPFClient{done: make(chan struct{})}
	cancel, err := startManagedPortForwardWithClient(context.Background(), client, "ns", "pod", []string{strconv.Itoa(port) + ":9999"})
	if err != nil {
		t.Fatal(err)
	}
	cancel()

	select {
	case <-client.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected port-forward worker to stop after cancel")
	}
}

func TestStartManagedPortForwardRejectsOccupiedLocalPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	client := &blockingPFClient{done: make(chan struct{})}
	cancel, err := startManagedPortForwardWithClient(context.Background(), client, "ns", "pod", []string{strconv.Itoa(port) + ":9999"})
	if cancel != nil {
		cancel()
	}
	if err == nil {
		t.Fatal("expected occupied local port error")
	}
	if !isPortListenConflict(err) {
		t.Fatalf("expected listen conflict error, got %v", err)
	}
}

func TestStartManagedPortForwardUsesParentContext(t *testing.T) {
	client := &blockingPFClient{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cancelPF, err := startManagedPortForwardWithClient(ctx, client, "ns", "pod", []string{"12345:9999"})
	if err == nil {
		if cancelPF != nil {
			cancelPF()
		}
		t.Fatal("expected canceled context error")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	select {
	case <-client.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected parent context cancellation to stop worker")
	}
}

func TestStartManagedPortForwardNoProbeUsesParentContext(t *testing.T) {
	client := &blockingPFClient{done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	cancelPF, err := startManagedPortForwardNoProbeWithClient(ctx, client, "ns", "pod", []string{"12345:9999"})
	if err == nil {
		if cancelPF != nil {
			cancelPF()
		}
		t.Fatal("expected canceled context error")
	}
	if err != context.Canceled {
		t.Fatalf("expected context.Canceled, got %v", err)
	}
	select {
	case <-client.done:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("expected parent context cancellation to stop worker")
	}
}

func TestAllPortsReachable(t *testing.T) {
	ln1, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln1.Close()
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln2.Close()

	p1 := ln1.Addr().(*net.TCPAddr).Port
	p2 := ln2.Addr().(*net.TCPAddr).Port
	if !allPortsReachable([]int{p1, p2}) {
		t.Fatal("expected both listening ports to be reachable")
	}
}
