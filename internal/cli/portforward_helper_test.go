package cli

import (
	"context"
	"io"
	"net"
	"strconv"
	"testing"
	"time"
)

type blockingPFClient struct {
	done chan struct{}
}

func (b *blockingPFClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	<-ctx.Done()
	close(b.done)
	return nil
}

func TestStartManagedPortForwardCancelWaitsForWorker(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	client := &blockingPFClient{done: make(chan struct{})}
	cancel, err := startManagedPortForwardWithClient(client, "ns", "pod", []string{strconv.Itoa(port) + ":9999"})
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
