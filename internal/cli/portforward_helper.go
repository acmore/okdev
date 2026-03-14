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

// startPortForwardKeepalive periodically creates a short-lived TCP connection
// against the local port-forward endpoint. This creates a fresh forwarded
// stream pair, which keeps the underlying SPDY/WebSocket session active
// without aggressively interacting with the remote protocol.
//
// If the local endpoint itself becomes unreachable repeatedly, the optional
// onDegraded callback is invoked to trigger reconnection.
func startPortForwardKeepalive(ctx context.Context, localPort int, interval time.Duration, onDegraded func()) {
	const maxConsecutiveFailures = 3

	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		consecutiveFailures := 0
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if probePortForward(localPort) {
					consecutiveFailures = 0
				} else {
					consecutiveFailures++
					slog.Debug("port-forward keepalive probe failed", "port", localPort, "consecutive", consecutiveFailures)
					if consecutiveFailures >= maxConsecutiveFailures && onDegraded != nil {
						slog.Debug("port-forward degraded, triggering reconnect", "port", localPort, "failures", consecutiveFailures)
						onDegraded()
						consecutiveFailures = 0
					}
				}
			}
		}
	}()
}

func probePortForward(localPort int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", localPort), 2*time.Second)
	if err != nil {
		slog.Debug("port-forward keepalive dial failed", "port", localPort, "error", err)
		return false
	}
	defer conn.Close()
	return true
}
