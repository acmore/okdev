package cli

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"sync/atomic"
	"time"

	"github.com/acmore/okdev/internal/logx"
)

// ErrProxyHealthDisconnect is a sentinel error returned when the proxy
// exits due to a health-check-triggered disconnection. It should be
// suppressed from stderr in main.go since the ssh client will report
// the disconnect naturally.
var ErrProxyHealthDisconnect = errors.New("proxy health disconnect")

// monitoredReader wraps an io.Reader and updates an atomic timestamp
// on every successful Read.
type monitoredReader struct {
	r        io.Reader
	lastData *atomic.Int64
}

func (m *monitoredReader) Read(p []byte) (int, error) {
	n, err := m.r.Read(p)
	if n > 0 {
		m.lastData.Store(time.Now().UnixNano())
	}
	return n, err
}

// monitoredCopy copies from src to dst, updating lastData on every
// successful read. Returns bytes copied and any error.
func monitoredCopy(dst io.Writer, src io.Reader, lastData *atomic.Int64) (int64, error) {
	return io.Copy(dst, &monitoredReader{r: src, lastData: lastData})
}

// proxyDataFlowWatchdog checks lastData every checkInterval. If no data
// has flowed for idleThreshold, it invokes onIdle and returns.
// It also returns when ctx is cancelled.
func proxyDataFlowWatchdog(ctx context.Context, lastData *atomic.Int64, checkInterval, idleThreshold time.Duration, onIdle func(idle time.Duration)) {
	ticker := time.NewTicker(checkInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			last := lastData.Load()
			if last == 0 {
				continue // no data yet, skip
			}
			idle := time.Since(time.Unix(0, last))
			if idle >= idleThreshold {
				if onIdle != nil {
					onIdle(idle)
				}
				return
			}
		}
	}
}

// setTCPKeepAliveProxyTuning reduces the TCP keepalive period and
// optionally sets TCP_KEEPCNT where supported.
func setTCPKeepAliveProxyTuning(conn net.Conn) {
	tc, ok := conn.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAlivePeriod(5 * time.Second)

	// Try to set TCP_KEEPCNT=2 via syscall. This is best-effort.
	raw, err := tc.SyscallConn()
	if err != nil {
		logx.Printf("time=%s source=ssh-proxy msg=%q err=%q\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), "failed to get syscall conn for TCP_KEEPCNT", err)
		slog.Debug("ssh-proxy: failed to get syscall conn for TCP_KEEPCNT", "error", err)
		return
	}
	if err := setTCPKeepCnt(raw, 2); err != nil {
		logx.Printf("time=%s source=ssh-proxy msg=%q err=%q\n", time.Now().Format("2006-01-02T15:04:05.000Z07:00"), "TCP_KEEPCNT not set", err)
		slog.Debug("ssh-proxy: TCP_KEEPCNT not set (unsupported or failed)", "error", err)
	}
}
