package cli

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
)

const portForwardShutdownWait = 2 * time.Second

func startManagedPortForward(opts *Options, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	return startManagedPortForwardWithClient(newKubeClient(opts), namespace, pod, forwards)
}

func startManagedPortForwardWithClient(k interface {
	PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		errCh <- k.PortForward(ctx, namespace, pod, forwards, io.Discard, io.Discard)
	}()
	cancelAndWait := func() {
		cancel()
		select {
		case <-doneCh:
		case <-time.After(portForwardShutdownWait):
		}
	}

	ports := localPortsFromForwards(forwards)
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-errCh:
			cancelAndWait()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("port-forward exited before ready")
		default:
		}

		if allPortsReachable(ports) {
			return cancelAndWait, nil
		}
		if time.Now().After(deadline) {
			cancelAndWait()
			return nil, fmt.Errorf("timeout waiting for port-forward readiness")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func localPortsFromForwards(forwards []string) []int {
	ports := make([]int, 0, len(forwards))
	for _, f := range forwards {
		parts := strings.Split(f, ":")
		if len(parts) != 2 {
			continue
		}
		p, err := strconv.Atoi(parts[0])
		if err != nil {
			continue
		}
		ports = append(ports, p)
	}
	return ports
}

func allPortsReachable(ports []int) bool {
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
	}
	return len(ports) > 0
}
