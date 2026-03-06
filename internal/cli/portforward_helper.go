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

func startManagedPortForward(opts *Options, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		errCh <- newKubeClient(opts).PortForward(ctx, namespace, pod, forwards, io.Discard, io.Discard)
	}()

	ports := localPortsFromForwards(forwards)
	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-errCh:
			cancel()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("port-forward exited before ready")
		default:
		}

		if allPortsReachable(ports) {
			return cancel, nil
		}
		if time.Now().After(deadline) {
			cancel()
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
