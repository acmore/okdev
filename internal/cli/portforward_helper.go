package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"time"
)

const portForwardShutdownWait = 2 * time.Second

func startManagedPortForward(opts *Options, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	return startManagedPortForwardWithClient(newKubeClient(opts), namespace, pod, forwards)
}

func startManagedPortForwardNoProbe(opts *Options, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	return startManagedPortForwardNoProbeWithClient(newKubeClient(opts), namespace, pod, forwards)
}

func startManagedPortForwardWithClient(k interface {
	PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		slog.Debug("starting port-forward worker", "pod", pod, "forwards", forwards)
		err := k.PortForward(ctx, namespace, pod, forwards, io.Discard, io.Discard)
		if err != nil {
			slog.Debug("port-forward worker exited with error", "pod", pod, "error", err)
		} else {
			slog.Debug("port-forward worker exited normally", "pod", pod)
		}
		errCh <- err
	}()
	cancelAndWait := func() {
		cancel()
		select {
		case <-doneCh:
		case <-time.After(portForwardShutdownWait):
		}
	}

	ports := localPortsFromForwards(forwards)
	readinessTicker := time.NewTicker(200 * time.Millisecond)
	defer readinessTicker.Stop()
	timeout := time.NewTimer(10 * time.Second)
	defer timeout.Stop()
	for {
		select {
		case err := <-errCh:
			cancelAndWait()
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("port-forward exited before ready")
		case <-readinessTicker.C:
			if allPortsReachable(ports) {
				return cancelAndWait, nil
			}
		case <-timeout.C:
			cancelAndWait()
			return nil, fmt.Errorf("timeout waiting for port-forward readiness")
		}
	}
}

func startManagedPortForwardNoProbeWithClient(k interface {
	PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}, namespace, pod string, forwards []string) (context.CancelFunc, error) {
	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	doneCh := make(chan struct{})
	go func() {
		defer close(doneCh)
		slog.Debug("starting port-forward worker (no-probe)", "pod", pod, "forwards", forwards)
		err := k.PortForward(ctx, namespace, pod, forwards, io.Discard, io.Discard)
		if err != nil {
			slog.Debug("port-forward worker (no-probe) exited with error", "pod", pod, "error", err)
		} else {
			slog.Debug("port-forward worker (no-probe) exited normally", "pod", pod)
		}
		errCh <- err
	}()
	cancelAndWait := func() {
		cancel()
		select {
		case <-doneCh:
		case <-time.After(portForwardShutdownWait):
		}
	}
	timer := time.NewTimer(500 * time.Millisecond)
	defer timer.Stop()
	select {
	case err := <-errCh:
		cancelAndWait()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf("port-forward exited before ready")
	case <-timer.C:
		return cancelAndWait, nil
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
	if len(ports) == 0 {
		return false
	}
	for _, p := range ports {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", p), 200*time.Millisecond)
		if err != nil {
			return false
		}
		_ = conn.Close()
	}
	return true
}

// startPortForwardKeepalive periodically dials the local port-forward endpoint
// to create new SPDY streams on the underlying connection. Each TCP connection
// triggers the port-forward handler to create a fresh stream pair (data+error),
// which generates SYN/FIN frames visible to all intermediaries (load balancers,
// API server proxies, WebSocket gateways) — keeping the connection alive even
// when they don't count data on existing streams as "activity".
func startPortForwardKeepalive(ctx context.Context, localPort int, interval time.Duration) {
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 2*time.Second)
				if err != nil {
					slog.Debug("port-forward keepalive dial failed", "port", localPort, "error", err)
					continue
				}
				_ = conn.Close()
			}
		}
	}()
}
