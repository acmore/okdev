package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/logx"
	"github.com/acmore/okdev/internal/session"
	okssh "github.com/acmore/okdev/internal/ssh"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

func newSSHCmd(opts *Options) *cobra.Command {
	var user string
	var localPort int
	var remotePort int
	var keyPath string
	var setupKey bool
	var cmdStr string
	var noTmux bool

	cmd := &cobra.Command{
		Use:   "ssh",
		Short: "Connect to session pod over SSH (dev container or okdev SSH sidecar)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return withQuietConfigAnnounce(func() error {
				errOut := cmd.ErrOrStderr()
				cfg, ns, err := loadConfigAndNamespace(opts)
				if err != nil {
					return err
				}
				stopResolve := startTransientStatus(errOut, "Resolving SSH session")
				sn, err := resolveSessionName(opts, cfg)
				stopResolve()
				if err != nil {
					return err
				}
				k := newKubeClient(opts)
				stopOwnership := startTransientStatus(errOut, "Verifying session access")
				if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
					stopOwnership()
					return err
				}
				stopOwnership()
				sshMode, modeErr := detectSessionSSHMode(opts, ns, sn)
				if modeErr != nil {
					return fmt.Errorf("detect ssh mode: %w", modeErr)
				}
				stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), true, true)
				defer stopMaintenance()

				if user == "" {
					user = cfg.Spec.SSH.User
				}
				if remotePort == 0 {
					remotePort = sshRemotePortForMode(cfg, sshMode)
				}
				if localPort == 0 {
					localPort = 2222
				}
				if keyPath == "" {
					keyPath, err = defaultSSHKeyPath(cfg)
					if err != nil {
						return err
					}
				}

				if setupKey {
					stopKeySetup := startTransientStatus(errOut, "Installing SSH key")
					if err := ensureSSHKeyOnPod(opts, cfg, ns, podName(sn), keyPath, sshMode); err != nil {
						stopKeySetup()
						return err
					}
					stopKeySetup()
				}
				stopSSHDReady := startTransientStatus(errOut, "Waiting for SSH service")
				if err := waitForSSHDReady(opts, cfg, ns, podName(sn), sshMode, 20*time.Second); err != nil {
					stopSSHDReady()
					fmt.Fprintf(errOut, "warning: sshd not ready yet: %v\n", err)
				} else {
					stopSSHDReady()
				}

				stopPortForward := startTransientStatus(errOut, "Starting SSH tunnel")
				cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(newKubeClient(opts), ns, podName(sn), localPort, remotePort)
				if err != nil {
					stopPortForward()
					return err
				}
				stopPortForward()
				pfKeepAliveCtx, pfKeepAliveCancel := context.WithCancel(cmd.Context())
				var tmRef atomic.Pointer[okssh.TunnelManager]
				pfDegraded := func() {
					if t := tmRef.Load(); t != nil {
						t.ForceReconnect()
					}
				}
				startPortForwardKeepalive(pfKeepAliveCtx, usedLocalPort, 10*time.Second, pfDegraded)
				var pfMu sync.Mutex
				currentCancelPF := cancelPF
				defer func() {
					pfKeepAliveCancel()
					pfMu.Lock()
					defer pfMu.Unlock()
					if currentCancelPF != nil {
						currentCancelPF()
					}
				}()

				sshHost := sshHostAlias(sn)
				cfgPath, _ := config.ResolvePath(opts.ConfigPath)
				stopSSHConfig := startTransientStatus(errOut, "Preparing SSH config")
				if _, cfgErr := ensureSSHConfigEntry(sshHost, sn, ns, user, remotePort, keyPath, cfgPath, cfg.Spec.Ports); cfgErr != nil {
					stopSSHConfig()
					fmt.Fprintf(errOut, "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
				} else {
					stopSSHConfig()
				}

				tm := &okssh.TunnelManager{
					SSHUser:           user,
					SSHKeyPath:        keyPath,
					RemotePort:        remotePort,
					KeepAliveInterval: time.Duration(cfg.Spec.SSH.KeepAliveInterval) * time.Second,
					KeepAliveTimeout:  time.Duration(cfg.Spec.SSH.KeepAliveTimeout) * time.Second,
					KeepAliveCountMax: cfg.Spec.SSH.KeepAliveCountMax,
				}
				if noTmux {
					tm.Env = map[string]string{"OKDEV_NO_TMUX": "1"}
				}
				tmRef.Store(tm)
				tm.SetReconnectTargetProvider(func(_ context.Context) (string, int, error) {
					pfMu.Lock()
					defer pfMu.Unlock()
					if currentCancelPF != nil {
						currentCancelPF()
						currentCancelPF = nil
					}
					pfKeepAliveCancel()
					reconnectLocalPort := localPort
					if p, perr := reserveEphemeralPort(); perr == nil {
						reconnectLocalPort = p
					}
					cancel, lp, err := startSSHPortForwardWithFallback(newKubeClient(opts), ns, podName(sn), reconnectLocalPort, remotePort)
					if err != nil {
						return "", 0, err
					}
					currentCancelPF = cancel
					newCtx, newCancel := context.WithCancel(cmd.Context())
					pfKeepAliveCancel = newCancel
					startPortForwardKeepalive(newCtx, lp, 10*time.Second, pfDegraded)
					return "127.0.0.1", lp, nil
				})
				var lastRTTWarnNanos atomic.Int64
				tm.SetKeepAliveRTTCallback(func(rtt time.Duration) {
					if rtt < 2*time.Second {
						return
					}
					now := time.Now().UnixNano()
					prev := lastRTTWarnNanos.Load()
					if prev != 0 && now-prev < int64(30*time.Second) {
						return
					}
					if lastRTTWarnNanos.CompareAndSwap(prev, now) {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: ssh keepalive RTT is high: %s\n", rtt.Round(10*time.Millisecond))
					}
				})
				stopConnect := startTransientStatus(errOut, "Connecting to session")
				if err := tm.Connect(cmd.Context(), "127.0.0.1", usedLocalPort); err != nil {
					stopConnect()
					return fmt.Errorf("connect ssh tunnel: %w", err)
				}
				stopConnect()
				defer tm.Close()

				if cmdStr != "" {
					out, err := tm.Exec(cmdStr)
					if len(out) > 0 {
						_, _ = cmd.OutOrStdout().Write(out)
					}
					if err != nil {
						return fmt.Errorf("ssh command failed: %w", err)
					}
					return nil
				}
				const maxRecoveryDuration = 3 * time.Minute
				rapidFailures := 0
				readinessFailures := 0
				var firstRapidFailure time.Time
				var recoveryStart time.Time
				inRecovery := false
				for {
					if requested, reqErr := session.ShutdownRequested(sn); reqErr == nil && requested {
						fmt.Fprintln(cmd.ErrOrStderr(), "Session shutdown requested. Closing SSH client.")
						return nil
					}
					started := time.Now()
					err := tm.OpenShell()
					if err == nil {
						return nil
					}
					if shouldReconnectShell(err, tm.IsConnected()) {
						if requested, reqErr := session.ShutdownRequested(sn); reqErr == nil && requested {
							fmt.Fprintln(cmd.ErrOrStderr(), "\nSession shutdown requested. Closing SSH client.")
							return nil
						}
						if !inRecovery {
							fmt.Fprintln(cmd.ErrOrStderr(), "\nConnection lost. Reconnecting...")
							inRecovery = true
							recoveryStart = time.Now()
						}
						if time.Since(recoveryStart) > maxRecoveryDuration {
							return fmt.Errorf("reconnect timed out after %s: %w", maxRecoveryDuration, err)
						}
						if time.Since(started) < 5*time.Second {
							if rapidFailures == 0 {
								firstRapidFailure = time.Now()
							}
							rapidFailures++
						} else {
							rapidFailures = 0
							readinessFailures = 0
							firstRapidFailure = time.Time{}
						}
						if rapidFailures >= 20 && !firstRapidFailure.IsZero() && time.Since(firstRapidFailure) <= 60*time.Second {
							return fmt.Errorf("ssh shell failed repeatedly after reconnect: %w", err)
						}
						waitCtx, waitCancel := context.WithTimeout(cmd.Context(), 15*time.Second)
						connected := tm.WaitConnected(waitCtx)
						waitCancel()
						if !connected {
							if cmd.Context().Err() != nil {
								fmt.Fprintln(cmd.ErrOrStderr(), "Reconnect cancelled. Session ended.")
								return nil
							}
							elapsed := time.Since(recoveryStart).Round(time.Second)
							fmt.Fprintf(cmd.ErrOrStderr(), "Still reconnecting (%s elapsed)...\n", elapsed)
							continue
						}
						if err := waitForSSHSessionReady(cmd.Context(), tm, 15*time.Second); err != nil {
							readinessFailures++
							elapsed := time.Since(recoveryStart).Round(time.Second)
							if readinessFailures == 1 || readinessFailures%5 == 0 {
								fmt.Fprintf(cmd.ErrOrStderr(), "Reconnect not ready yet (%s elapsed): %v\n", elapsed, err)
							}
							tm.ForceReconnect()
							continue
						}
						readinessFailures = 0
						fmt.Fprintln(cmd.ErrOrStderr(), "Reconnected.")
						inRecovery = false
						continue
					}
					return fmt.Errorf("ssh shell failed: %w", err)
				}
			})
		},
	}

	cmd.Flags().StringVar(&user, "user", "", "SSH username")
	cmd.Flags().IntVar(&localPort, "local-port", 0, "Local port for SSH tunnel")
	cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote SSH port in pod")
	cmd.Flags().StringVar(&keyPath, "key", "", "Private key path")
	cmd.Flags().BoolVar(&setupKey, "setup-key", false, "Generate local key if needed and install its public key in pod")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Run a command over SSH")
	cmd.Flags().BoolVar(&noTmux, "no-tmux", false, "Disable tmux for this SSH session even if enabled on the pod")
	return cmd
}

func ensureSSHKeyOnPod(opts *Options, cfg *config.DevEnvironment, namespace, pod, keyPath, sshMode string) error {
	if err := ensureCommand("ssh-keygen"); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(keyPath), 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(keyPath); errors.Is(err, os.ErrNotExist) {
		gen := exec.Command("ssh-keygen", "-t", "ed25519", "-N", "", "-f", keyPath)
		if out, err := gen.CombinedOutput(); err != nil {
			return fmt.Errorf("generate ssh key: %w (%s)", err, strings.TrimSpace(string(out)))
		}
	}

	pubKeyBytes, err := os.ReadFile(keyPath + ".pub")
	if err != nil {
		return fmt.Errorf("read public key: %w", err)
	}
	pubKey := strings.TrimSpace(string(pubKeyBytes))
	if pubKey == "" {
		return errors.New("public key is empty")
	}

	script := fmt.Sprintf("mkdir -p ~/.ssh && chmod 700 ~/.ssh && touch ~/.ssh/authorized_keys && chmod 600 ~/.ssh/authorized_keys && (grep -qxF %s ~/.ssh/authorized_keys || echo %s >> ~/.ssh/authorized_keys)", syncengine.ShellEscape(pubKey), syncengine.ShellEscape(pubKey))
	k := newKubeClient(opts)
	container := sshTargetContainer(cfg)
	var lastErr error
	installed := false
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if container == "" {
			_, err = k.ExecSh(ctx, namespace, pod, script)
		} else {
			_, err = k.ExecShInContainer(ctx, namespace, pod, container, script)
		}
		cancel()
		if err == nil {
			installed = true
			break
		}
		lastErr = err
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	if !installed {
		return fmt.Errorf("install ssh key in pod: %w", lastErr)
	}

	if normalizeSSHMode(sshMode) == sshModeEmbedded {
		copyScript := `set -eu
DEV_PID=""
for pid in $(ls /proc 2>/dev/null | grep -E '^[0-9]+$' | sort -n); do
  [ "$pid" = "1" ] && continue
  [ "$pid" = "$$" ] && continue
  [ -r "/proc/$pid/environ" ] 2>/dev/null || continue
  if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -qx 'OKDEV_CONTAINER_ROLE=dev'; then
    DEV_PID="$pid"
    break
  fi
done
[ -n "$DEV_PID" ]
nsenter --target "$DEV_PID" --mount -- mkdir -p /var/okdev
cat /root/.ssh/authorized_keys | nsenter --target "$DEV_PID" --mount -- sh -c "cat > /var/okdev/authorized_keys && chmod 600 /var/okdev/authorized_keys"`
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_, err := k.ExecShInContainer(ctx, namespace, pod, "okdev-sidecar", copyScript)
		cancel()
		if err != nil {
			return fmt.Errorf("sync embedded ssh authorized_keys: %w", err)
		}
	}
	return nil
}

func waitForSSHDReady(opts *Options, cfg *config.DevEnvironment, namespace, pod, sshMode string, timeout time.Duration) error {
	k := newKubeClient(opts)
	container := sshTargetContainer(cfg)
	if container == "" {
		return nil
	}
	probeCmd := "ps | grep '[s]shd' >/dev/null 2>&1"
	if normalizeSSHMode(sshMode) == sshModeEmbedded {
		probeCmd = "ps | grep '[o]kdev-sshd' >/dev/null 2>&1"
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := k.ExecShInContainer(ctx, namespace, pod, container, probeCmd)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(300 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout waiting for sshd")
	}
	return fmt.Errorf("wait for sshd ready: %w", lastErr)
}

func defaultSSHKeyPath(cfg *config.DevEnvironment) (string, error) {
	keyPath := cfg.Spec.SSH.PrivateKeyPath
	if keyPath != "" {
		return keyPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	legacy := filepath.Join(home, ".ssh", "okdev_ed25519")
	if _, err := os.Stat(legacy); err == nil {
		return legacy, nil
	}
	return filepath.Join(home, ".okdev", "ssh", "id_ed25519"), nil
}

func sshTargetContainer(cfg *config.DevEnvironment) string {
	if cfg == nil {
		return ""
	}
	// Sidecar is always used; target the merged okdev-sidecar container.
	return "okdev-sidecar"
}

func sshHostAlias(sessionName string) string {
	return "okdev-" + sessionName
}

func ensureSSHConfigEntry(hostAlias, sessionName, namespace, user string, remotePort int, keyPath, okdevConfigPath string, forwards []config.PortMapping) (bool, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return false, err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return false, err
	}
	sshConfigPath := filepath.Join(sshDir, "config")
	begin := "# BEGIN OKDEV " + hostAlias
	end := "# END OKDEV " + hostAlias
	proxyInner := fmt.Sprintf("okdev --session %s -n %s ssh-proxy --remote-port %d", shellQuote(sessionName), shellQuote(namespace), remotePort)
	if strings.TrimSpace(okdevConfigPath) != "" {
		proxyInner += " -c " + shellQuote(okdevConfigPath)
	}
	proxyCmd := "sh -lc " + shellQuote(proxyInner)
	blockLines := []string{
		begin,
		"Host " + hostAlias,
		"  HostName 127.0.0.1",
		fmt.Sprintf("  Port %d", remotePort),
		"  User " + user,
		"  IdentityFile " + keyPath,
		"  StrictHostKeyChecking no",
		"  UserKnownHostsFile /dev/null",
		"  ProxyCommand " + proxyCmd,
	}
	// Hardcoded keepalive values for the proxy path — sshSpec values are
	// intentionally ignored here because the proxy needs tighter detection
	// than the general SSH config defaults provide.
	blockLines = append(blockLines,
		"  ServerAliveInterval 5",
		"  ServerAliveCountMax 10",
		"  TCPKeepAlive yes",
		"  LogLevel ERROR",
		end,
	)
	block := strings.Join(blockLines, "\n") + "\n"
	lockPath := sshConfigPath + ".okdev.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return false, err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	existingBytes, _ := os.ReadFile(sshConfigPath)
	existing := string(existingBytes)
	updated := strings.TrimSpace(stripManagedSSHBlock(existing, begin, end)+"\n"+block) + "\n"
	if updated == existing {
		return false, nil
	}
	if err := os.WriteFile(sshConfigPath, []byte(updated), 0o600); err != nil {
		return false, err
	}
	return true, nil
}

func isManagedSSHForwardRunning(hostAlias string) (bool, error) {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return false, err
	}
	check := exec.Command("ssh", "-S", socketPath, "-O", "check", hostAlias)
	if err := check.Run(); err == nil {
		return true, nil
	}
	return false, nil
}

func stripManagedSSHBlock(content, begin, end string) string {
	lines := strings.Split(content, "\n")
	out := make([]string, 0, len(lines))
	inBlock := false
	for _, line := range lines {
		t := strings.TrimSpace(line)
		if t == begin {
			inBlock = true
			continue
		}
		if inBlock {
			if t == end {
				inBlock = false
			}
			continue
		}
		out = append(out, line)
	}
	for len(out) > 0 && strings.TrimSpace(out[len(out)-1]) == "" {
		out = out[:len(out)-1]
	}
	return strings.Join(out, "\n")
}

func shellQuote(v string) string {
	if v == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(v, "'", `'"'"'`) + "'"
}

func newSSHProxyCmd(opts *Options) *cobra.Command {
	var remotePort int
	cmd := &cobra.Command{
		Use:    "ssh-proxy",
		Short:  "Internal SSH proxy bridge",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withQuietConfigAnnounce(func() error {
				cfg, ns, err := loadConfigAndNamespace(opts)
				if err != nil {
					return err
				}
				sn, err := resolveSessionName(opts, cfg)
				if err != nil {
					return err
				}
				k := newKubeClient(opts)
				if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
					return err
				}
				if remotePort == 0 {
					remotePort = cfg.Spec.SSH.RemotePort
				}
				cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(k, ns, podName(sn), 2222, remotePort)
				if err != nil {
					return err
				}
				if cancelPF != nil {
					defer cancelPF()
				}

				slog.Debug("ssh-proxy dialing local port-forward", "port", usedLocalPort)
				conn, err := waitDialLocal(usedLocalPort, 10*time.Second)
				if err != nil {
					slog.Debug("ssh-proxy dial failed", "error", err)
					return err
				}
				defer conn.Close()
				startTime := time.Now()
				var healthDisconnect atomic.Bool

				// Layer 1: TCP keepalive tuning (5s period, KEEPCNT=2 where supported)
				setTCPKeepAliveProxyTuning(conn)

				// Layer 3: Port-forward keepalive probe with onDegraded wired up
				pfKeepAliveCtx, pfKeepAliveCancel := context.WithCancel(cmd.Context())
				defer pfKeepAliveCancel()
				startPortForwardKeepalive(pfKeepAliveCtx, usedLocalPort, 10*time.Second, func() {
					healthDisconnect.Store(true)
					logx.Printf("time=%s source=ssh-proxy msg=%q port=%d uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"port-forward degraded, closing connection",
						usedLocalPort,
						time.Since(startTime).Round(time.Second),
					)
					_ = conn.Close()
				})

				slog.Debug("ssh-proxy connection established", "localAddr", conn.LocalAddr(), "remoteAddr", conn.RemoteAddr())

				// Close conn on context cancellation (SIGINT/SIGTERM) to unblock io.Copy goroutines
				go func() {
					<-cmd.Context().Done()
					_ = conn.Close()
				}()

				// Layer 2: Data flow monitor
				var lastData atomic.Int64
				lastData.Store(time.Now().UnixNano())
				watchdogCtx, watchdogCancel := context.WithCancel(cmd.Context())
				defer watchdogCancel()
				go proxyDataFlowWatchdog(watchdogCtx, &lastData, 5*time.Second, 15*time.Second, func(idle time.Duration) {
					healthDisconnect.Store(true)
					logx.Printf("time=%s source=ssh-proxy msg=%q idle=%s threshold=%s uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"data flow watchdog idle timeout",
						idle.Round(time.Millisecond),
						(15 * time.Second),
						time.Since(startTime).Round(time.Second),
					)
					_ = conn.Close()
				})

				var copyErr error
				var once sync.Once
				done := make(chan struct{})
				setErr := func(err error) {
					if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || isIgnorableProxyIOError(err) {
						return
					}
					slog.Debug("ssh-proxy copy error", "error", err)
					once.Do(func() { copyErr = err })
				}
				go func() {
					slog.Debug("ssh-proxy starting copy: stdin -> conn")
					_, err := monitoredCopy(conn, os.Stdin, &lastData)
					setErr(err)
					slog.Debug("ssh-proxy finished copy: stdin -> conn")
					select {
					case <-done:
					default:
						_ = conn.Close()
					}
				}()
				go func() {
					slog.Debug("ssh-proxy starting copy: conn -> stdout")
					_, err := monitoredCopy(os.Stdout, conn, &lastData)
					setErr(err)
					slog.Debug("ssh-proxy finished copy: conn -> stdout")
					close(done)
					_ = conn.Close()
				}()
				<-done
				watchdogCancel()
				if healthDisconnect.Load() {
					logx.Printf("time=%s source=ssh-proxy msg=%q err=%q uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"session finished after health-triggered disconnect",
						copyErr,
						time.Since(startTime).Round(time.Second),
					)
					if copyErr != nil {
						return fmt.Errorf("%w: %v", ErrProxyHealthDisconnect, copyErr)
					}
					return ErrProxyHealthDisconnect
				}
				if copyErr != nil {
					logx.Printf("time=%s source=ssh-proxy msg=%q err=%q uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"session finished with proxy copy error",
						copyErr,
						time.Since(startTime).Round(time.Second),
					)
					return copyErr
				}
				return nil
			})
		},
	}
	cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote SSH port in pod")
	return cmd
}

func isIgnorableProxyIOError(err error) bool {
	if err == nil {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "broken pipe") ||
		strings.Contains(msg, "use of closed network connection") ||
		strings.Contains(msg, "connection reset by peer") ||
		strings.Contains(msg, "remote command exited without exit status")
}

func shouldReconnectShell(err error, connected bool) bool {
	if err == nil {
		return false
	}
	if !connected || isIgnorableProxyIOError(err) {
		return true
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "eof") ||
		strings.Contains(msg, "timeout") ||
		strings.Contains(msg, "closed by remote host") ||
		strings.Contains(msg, "connection closed") ||
		strings.Contains(msg, "i/o timeout")
}

func waitForSSHSessionReady(ctx context.Context, tm *okssh.TunnelManager, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	consecutiveSuccess := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		_, err := tm.ExecContext(probeCtx, "true")
		cancel()
		if err == nil {
			consecutiveSuccess++
			if consecutiveSuccess >= 2 {
				return nil
			}
			time.Sleep(150 * time.Millisecond)
			continue
		}
		consecutiveSuccess = 0
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout waiting for ssh session readiness")
	}
	return lastErr
}

func waitDialLocal(localPort int, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(10 * time.Second)
			}
			return conn, nil
		}
		lastErr = err
		time.Sleep(150 * time.Millisecond)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timed out connecting to %s", addr)
	}
	return nil, lastErr
}

func sshControlSocketPath(hostAlias string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".okdev", "ssh")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(dir, hostAlias+".sock"), nil
}

func startManagedSSHForward(hostAlias string, sshSpec config.SSHSpec) error {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return err
	}
	check := exec.Command("ssh", "-S", socketPath, "-O", "check", hostAlias)
	if err := check.Run(); err == nil {
		return nil
	}
	return startManagedSSHForwardWithForwards(hostAlias, nil, sshSpec)
}

func startManagedSSHForwardWithForwards(hostAlias string, forwards []config.PortMapping, sshSpec config.SSHSpec) error {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return err
	}
	check := exec.Command("ssh", "-S", socketPath, "-O", "check", hostAlias)
	if err := check.Run(); err == nil {
		return nil
	}
	args := []string{
		"ssh",
		"-fN",
		"-M",
		"-S", socketPath,
		"-o", "ControlPersist=3600",
		"-o", "ExitOnForwardFailure=no",
		"-o", fmt.Sprintf("ServerAliveInterval=%d", sshSpec.KeepAliveInterval),
		"-o", fmt.Sprintf("ServerAliveCountMax=%d", sshSpec.KeepAliveCountMax),
		"-o", "TCPKeepAlive=yes",
		"-o", "LogLevel=ERROR",
	}
	for _, p := range forwards {
		if p.Local <= 0 || p.Remote <= 0 {
			continue
		}
		args = append(args, "-L", fmt.Sprintf("%d:127.0.0.1:%d", p.Local, p.Remote))
	}
	args = append(args, hostAlias)
	cmd := exec.Command(args[0], args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start managed ssh forward: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func stopManagedSSHForward(hostAlias string) error {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return err
	}
	cmd := exec.Command("ssh", "-S", socketPath, "-O", "exit", hostAlias)
	if out, err := cmd.CombinedOutput(); err != nil {
		msg := strings.ToLower(strings.TrimSpace(string(out)))
		if strings.Contains(msg, "no such file") || strings.Contains(msg, "control socket connect") || strings.Contains(msg, "master running") {
			return nil
		}
		return fmt.Errorf("stop managed ssh forward: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func startSSHPortForwardWithFallback(k interface {
	PortForward(context.Context, string, string, []string, io.Writer, io.Writer) error
}, namespace, pod string, preferredLocalPort, remotePort int) (context.CancelFunc, int, error) {
	tryPorts := []int{preferredLocalPort}
	if preferredLocalPort > 0 {
		if p, err := reserveEphemeralPort(); err == nil {
			tryPorts = append(tryPorts, p)
		}
	}
	var lastErr error
	for _, lp := range tryPorts {
		cancel, err := startManagedPortForwardWithClient(k, namespace, pod, []string{fmt.Sprintf("%d:%d", lp, remotePort)})
		if err == nil {
			return cancel, lp, nil
		}
		lastErr = err
		if !isPortListenConflict(err) {
			return nil, 0, err
		}
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("failed to start ssh port-forward")
	}
	return nil, 0, lastErr
}

func isPortListenConflict(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "address already in use") ||
		strings.Contains(msg, "unable to listen on any of the requested ports")
}

func reserveEphemeralPort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	addr, ok := ln.Addr().(*net.TCPAddr)
	if !ok {
		return 0, fmt.Errorf("unexpected listener address type %T", ln.Addr())
	}
	return addr.Port, nil
}

func updateSSHConfigWithLock(configPath string, mutator func(existing string) string) error {
	lockPath := configPath + ".okdev.lock"
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lockFile.Close()
	if err := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
	}()

	existing, _ := os.ReadFile(configPath)
	updated := mutator(string(existing))
	return os.WriteFile(configPath, []byte(updated), 0o600)
}
