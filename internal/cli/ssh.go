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
	"github.com/acmore/okdev/internal/shellutil"
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
	var forwardAgent bool
	var noForwardAgent bool

	cmd := &cobra.Command{
		Use:   "ssh [session]",
		Short: "Connect to session pod over SSH",
		Example: `  # Open a tmux-backed SSH shell
  okdev ssh

  # Connect to a specific session
  okdev ssh my-feature

  # Run a command over SSH
  okdev ssh --cmd "ls -la /workspace"

  # Plain shell without tmux
  okdev ssh --no-tmux

  # Forward local SSH agent into the session
  okdev ssh --forward-agent

  # Or use the managed SSH host alias directly
  ssh okdev-my-feature`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			return withQuietConfigAnnounce(func() error {
				errOut := cmd.ErrOrStderr()
				applySessionArg(opts, args)
				stopResolve := startTransientStatus(errOut, "Resolving SSH session")
				cc, err := resolveCommandContext(opts, resolveSessionName)
				stopResolve()
				if err != nil {
					return err
				}
				stopOwnership := startTransientStatus(errOut, "Verifying session access")
				if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
					stopOwnership()
					return err
				}
				stopOwnership()
				target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
				if err != nil {
					return err
				}
				stopMaintenance := startSessionMaintenance(opts, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
				defer stopMaintenance()

				if user == "" {
					user = cc.cfg.Spec.SSH.User
				}
				if remotePort == 0 {
					remotePort = sshPort
				}
				if localPort == 0 {
					localPort = 2222
				}
				if keyPath == "" {
					keyPath, err = defaultSSHKeyPath(cc.cfg)
					if err != nil {
						return err
					}
				}

				if setupKey {
					stopKeySetup := startTransientStatus(errOut, "Installing SSH key")
					if err := ensureSSHKeyOnPod(opts, cc.namespace, target.PodName, target.Container, keyPath); err != nil {
						stopKeySetup()
						return err
					}
					stopKeySetup()
				}
				stopSSHDReady := startTransientStatus(errOut, "Waiting for SSH service")
				if err := waitForSSHReady(opts, cc.namespace, target.PodName, target.Container, sshServiceWaitTimeout); err != nil {
					stopSSHDReady()
					fmt.Fprintf(errOut, "warning: ssh service not ready yet (%v); continuing with SSH tunnel setup anyway\n", err)
				} else {
					stopSSHDReady()
				}

				stopPortForward := startTransientStatus(errOut, "Starting SSH tunnel")
				cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(cc.kube, cc.namespace, target.PodName, localPort, remotePort)
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
				startPortForwardKeepalive(pfKeepAliveCtx, usedLocalPort, portForwardKeepaliveInterval, pfDegraded)
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

				sshHost := sshHostAlias(cc.sessionName)
				stopSSHConfig := startTransientStatus(errOut, "Preparing SSH config")
				if _, cfgErr := ensureSSHConfigEntry(sshHost, cc.sessionName, cc.namespace, user, remotePort, keyPath, cc.cfgPath, cc.cfg.Spec.Ports); cfgErr != nil {
					stopSSHConfig()
					fmt.Fprintf(errOut, "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
				} else {
					stopSSHConfig()
				}

				warnSyncHealth(errOut, cc.sessionName)

				forwardAgentEnabled := effectiveSSHForwardAgent(cc.cfg, forwardAgent, noForwardAgent)
				agentSock := ""
				if forwardAgentEnabled {
					agentSock, err = resolveLocalSSHAgentSocket()
					if err != nil {
						return err
					}
				}

				tm := &okssh.TunnelManager{
					SSHUser:           user,
					SSHKeyPath:        keyPath,
					ForwardAgentSock:  agentSock,
					RemotePort:        remotePort,
					KeepAliveInterval: time.Duration(cc.cfg.Spec.SSH.KeepAliveInterval) * time.Second,
					KeepAliveTimeout:  time.Duration(cc.cfg.Spec.SSH.KeepAliveTimeout) * time.Second,
					KeepAliveCountMax: cc.cfg.Spec.SSH.KeepAliveCountMax,
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
					cancel, lp, err := startSSHPortForwardWithFallback(cc.kube, cc.namespace, target.PodName, reconnectLocalPort, remotePort)
					if err != nil {
						return "", 0, err
					}
					currentCancelPF = cancel
					newCtx, newCancel := context.WithCancel(cmd.Context())
					pfKeepAliveCancel = newCancel
					startPortForwardKeepalive(newCtx, lp, portForwardKeepaliveInterval, pfDegraded)
					return "127.0.0.1", lp, nil
				})
				var lastRTTWarnNanos atomic.Int64
				tm.SetKeepAliveRTTCallback(func(rtt time.Duration) {
					if rtt < sshRTTWarnThreshold {
						return
					}
					now := time.Now().UnixNano()
					prev := lastRTTWarnNanos.Load()
					if prev != 0 && now-prev < int64(sshRTTWarnCooldown) {
						return
					}
					if lastRTTWarnNanos.CompareAndSwap(prev, now) {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: ssh keepalive RTT is very high: %s\n", rtt.Round(10*time.Millisecond))
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
				return runSSHShellWithReconnect(cmd.Context(), tm, cc.sessionName, cmd.ErrOrStderr())
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
	cmd.Flags().BoolVar(&forwardAgent, "forward-agent", false, "Forward the local SSH agent into this SSH session")
	cmd.Flags().BoolVar(&noForwardAgent, "no-forward-agent", false, "Disable SSH agent forwarding for this SSH session")
	cmd.MarkFlagsMutuallyExclusive("forward-agent", "no-forward-agent")
	return cmd
}

func effectiveSSHForwardAgent(cfg *config.DevEnvironment, forwardAgent, noForwardAgent bool) bool {
	if noForwardAgent {
		return false
	}
	if forwardAgent {
		return true
	}
	if cfg == nil {
		return false
	}
	return cfg.Spec.SSH.ForwardAgentEnabled()
}

func resolveLocalSSHAgentSocket() (string, error) {
	sock := strings.TrimSpace(os.Getenv("SSH_AUTH_SOCK"))
	if sock == "" {
		return "", errors.New("ssh agent forwarding requested, but SSH_AUTH_SOCK is not set")
	}
	info, err := os.Stat(sock)
	if err != nil {
		return "", fmt.Errorf("ssh agent forwarding requested, but local SSH_AUTH_SOCK %q is unavailable: %w", sock, err)
	}
	if info.Mode()&os.ModeSocket == 0 {
		return "", fmt.Errorf("ssh agent forwarding requested, but SSH_AUTH_SOCK %q is not a unix socket", sock)
	}
	return sock, nil
}

// sshShellRunner abstracts the TunnelManager methods needed by
// runSSHShellWithReconnect so the reconnection logic can be tested
// without a real SSH connection.
type sshShellRunner interface {
	OpenShell() error
	IsConnected() bool
	WaitConnected(ctx context.Context) bool
	ForceReconnect()
	ExecContext(ctx context.Context, cmd string) ([]byte, error)
}

const rapidFailureThreshold = 20

func runSSHShellWithReconnect(ctx context.Context, tm sshShellRunner, sessionName string, errOut io.Writer) error {
	rapidFailures := 0
	readinessFailures := 0
	var firstRapidFailure time.Time
	var recoveryStart time.Time
	inRecovery := false
	for {
		if requested, reqErr := session.ShutdownRequested(sessionName); reqErr == nil && requested {
			fmt.Fprintln(errOut, "Session shutdown requested. Closing SSH client.")
			return nil
		}
		started := time.Now()
		err := tm.OpenShell()
		if err == nil {
			return nil
		}
		if !shouldReconnectShell(err, tm.IsConnected()) {
			return fmt.Errorf("ssh shell failed: %w", err)
		}
		if requested, reqErr := session.ShutdownRequested(sessionName); reqErr == nil && requested {
			fmt.Fprintln(errOut, "\nSession shutdown requested. Closing SSH client.")
			return nil
		}
		if !inRecovery {
			fmt.Fprintln(errOut, "\nConnection lost. Reconnecting...")
			inRecovery = true
			recoveryStart = time.Now()
		}
		if time.Since(recoveryStart) > sshRecoveryTimeout {
			return fmt.Errorf("reconnect timed out after %s: %w", sshRecoveryTimeout, err)
		}
		if time.Since(started) < sshShellStartThreshold {
			if rapidFailures == 0 {
				firstRapidFailure = time.Now()
			}
			rapidFailures++
		} else {
			rapidFailures = 0
			readinessFailures = 0
			firstRapidFailure = time.Time{}
		}
		if rapidFailures >= rapidFailureThreshold && !firstRapidFailure.IsZero() && time.Since(firstRapidFailure) <= sshRapidFailureWindow {
			return fmt.Errorf("ssh shell failed repeatedly after reconnect: %w", err)
		}
		waitCtx, waitCancel := context.WithTimeout(ctx, sshReconnectWaitTimeout)
		connected := tm.WaitConnected(waitCtx)
		waitCancel()
		if !connected {
			if ctx.Err() != nil {
				fmt.Fprintln(errOut, "Reconnect cancelled. Session ended.")
				return nil
			}
			elapsed := time.Since(recoveryStart).Round(time.Second)
			fmt.Fprintf(errOut, "Still reconnecting (%s elapsed)...\n", elapsed)
			continue
		}
		if err := waitForSSHSessionReady(ctx, tm, sshReadinessTimeout); err != nil {
			readinessFailures++
			elapsed := time.Since(recoveryStart).Round(time.Second)
			if readinessFailures == 1 || readinessFailures%5 == 0 {
				fmt.Fprintf(errOut, "Reconnect not ready yet (%s elapsed): %v\n", elapsed, err)
			}
			tm.ForceReconnect()
			continue
		}
		readinessFailures = 0
		fmt.Fprintln(errOut, "Reconnected.")
		inRecovery = false
	}
}

func ensureSSHKeyOnPod(opts *Options, namespace, pod, container, keyPath string) error {
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
	var lastErr error
	installed := false
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), sshKeyInstallTimeout)
		_, err = k.ExecShInContainer(ctx, namespace, pod, container, script)
		cancel()
		if err == nil {
			installed = true
			break
		}
		lastErr = err
		time.Sleep(time.Duration(i+1) * sshKeyInstallRetryStep)
	}
	if !installed {
		return fmt.Errorf("install ssh key in pod: %w", lastErr)
	}
	return nil
}

func waitForSSHReady(opts *Options, namespace, pod, container string, timeout time.Duration) error {
	k := newKubeClient(opts)
	if err := ensureDevSSHDRunning(context.Background(), k, namespace, pod, container); err != nil {
		return fmt.Errorf("start dev-container sshd: %w", err)
	}
	probeCmd := "ps | grep '[o]kdev-sshd' >/dev/null 2>&1"
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), execProbeTimeout)
		_, err := k.ExecShInContainer(ctx, namespace, pod, container, probeCmd)
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(sshdReadinessPollInterval)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("timeout waiting for sshd")
	}
	return fmt.Errorf("wait for sshd ready: %w", lastErr)
}

const devSSHStartScript = `set -eu
mkdir -p /var/okdev "$HOME/.ssh"
if [ ! -x /var/okdev/okdev-sshd ]; then
  echo "missing /var/okdev/okdev-sshd" >&2
  exit 1
fi
if ps | grep '[o]kdev-sshd' >/dev/null 2>&1; then
  exit 0
fi
export OKDEV_WORKSPACE="${OKDEV_WORKSPACE:-/workspace}"
export OKDEV_TMUX="${OKDEV_TMUX:-0}"
nohup /var/okdev/okdev-sshd --port 2222 --authorized-keys "$HOME/.ssh/authorized_keys" >/var/okdev/okdev-sshd.log 2>&1 </dev/null &
`

type devSSHStarter interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

func ensureDevSSHDRunning(ctx context.Context, k devSSHStarter, namespace, pod, container string) error {
	startCtx, cancel := context.WithTimeout(ctx, sshdStartTimeout)
	defer cancel()
	_, err := k.ExecShInContainer(startCtx, namespace, pod, container, devSSHStartScript)
	return err
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

func sshHostAlias(sessionName string) string {
	return "okdev-" + sessionName
}

func sshConfigPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return "", err
	}
	return filepath.Join(sshDir, "config"), nil
}

func ensureSSHConfigEntry(hostAlias, sessionName, namespace, user string, remotePort int, keyPath, okdevConfigPath string, forwards []config.PortMapping) (bool, error) {
	sshConfigPath, err := sshConfigPath()
	if err != nil {
		return false, err
	}
	okdevExecPath, err := os.Executable()
	if err != nil {
		return false, err
	}
	begin := "# BEGIN OKDEV " + hostAlias
	end := "# END OKDEV " + hostAlias
	proxyInner := fmt.Sprintf("%s --session %s -n %s ssh-proxy --remote-port %d", shellQuote(okdevExecPath), shellQuote(sessionName), shellQuote(namespace), remotePort)
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
		"  SetEnv OKDEV_NO_TMUX=1",
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
	defer func() {
		_ = os.Remove(lockPath)
	}()
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
	return shellutil.Quote(v)
}

func newSSHProxyCmd(opts *Options) *cobra.Command {
	var remotePort int
	cmd := &cobra.Command{
		Use:    "ssh-proxy",
		Short:  "Internal SSH proxy bridge",
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			return withQuietConfigAnnounce(func() error {
				cc, err := resolveCommandContext(opts, resolveSessionName)
				if err != nil {
					return err
				}
				if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
					return err
				}
				target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
				if err != nil {
					return err
				}
				if remotePort == 0 {
					remotePort = sshPort
				}
				cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(cc.kube, cc.namespace, target.PodName, 2222, remotePort)
				if err != nil {
					return err
				}
				if cancelPF != nil {
					defer cancelPF()
				}

				slog.Debug("ssh-proxy dialing local port-forward", "port", usedLocalPort)
				conn, err := waitDialLocal(usedLocalPort, localDialTotalTimeout)
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
				startPortForwardKeepalive(pfKeepAliveCtx, usedLocalPort, portForwardKeepaliveInterval, func() {
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
				go proxyDataFlowWatchdog(watchdogCtx, &lastData, dataFlowWatchdogInterval, dataFlowWatchdogIdleTimeout, func(idle time.Duration) {
					healthDisconnect.Store(true)
					logx.Printf("time=%s source=ssh-proxy msg=%q idle=%s threshold=%s uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"data flow watchdog idle timeout",
						idle.Round(time.Millisecond),
						dataFlowWatchdogIdleTimeout,
						time.Since(startTime).Round(time.Second),
					)
					_ = conn.Close()
				})

				var copyErr error
				var copyErrMu sync.Mutex
				var once sync.Once
				done := make(chan struct{})
				getCopyErr := func() error {
					copyErrMu.Lock()
					defer copyErrMu.Unlock()
					return copyErr
				}
				setErr := func(err error) {
					if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || isIgnorableProxyIOError(err) {
						return
					}
					slog.Debug("ssh-proxy copy error", "error", err)
					once.Do(func() {
						copyErrMu.Lock()
						copyErr = err
						copyErrMu.Unlock()
					})
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
					err := getCopyErr()
					logx.Printf("time=%s source=ssh-proxy msg=%q err=%q uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"session finished after health-triggered disconnect",
						err,
						time.Since(startTime).Round(time.Second),
					)
					if err != nil {
						return fmt.Errorf("%w: %v", ErrProxyHealthDisconnect, err)
					}
					return ErrProxyHealthDisconnect
				}
				if err := getCopyErr(); err != nil {
					logx.Printf("time=%s source=ssh-proxy msg=%q err=%q uptime=%s\n",
						time.Now().Format("2006-01-02T15:04:05.000Z07:00"),
						"session finished with proxy copy error",
						err,
						time.Since(startTime).Round(time.Second),
					)
					return err
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

type sshSessionProber interface {
	ExecContext(ctx context.Context, cmd string) ([]byte, error)
}

func waitForSSHSessionReady(ctx context.Context, tm sshSessionProber, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	var lastErr error
	consecutiveSuccess := 0
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		probeCtx, cancel := context.WithTimeout(ctx, sshReadinessProbeTimeout)
		_, err := tm.ExecContext(probeCtx, "true")
		cancel()
		if err == nil {
			consecutiveSuccess++
			if consecutiveSuccess >= 2 {
				return nil
			}
			time.Sleep(sshReadinessSuccessWait)
			continue
		}
		consecutiveSuccess = 0
		lastErr = err
		time.Sleep(sshReadinessFailureWait)
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
		conn, err := net.DialTimeout("tcp", addr, localDialTimeout)
		if err == nil {
			if tc, ok := conn.(*net.TCPConn); ok {
				_ = tc.SetKeepAlive(true)
				_ = tc.SetKeepAlivePeriod(proxyTCPKeepAlivePeriod)
			}
			return conn, nil
		}
		lastErr = err
		time.Sleep(localDialRetryInterval)
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
	configPath, err := sshConfigPath()
	if err != nil {
		return err
	}
	check := exec.Command("ssh", "-F", configPath, "-S", socketPath, "-O", "check", hostAlias)
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
	configPath, err := sshConfigPath()
	if err != nil {
		return err
	}
	check := exec.Command("ssh", "-F", configPath, "-S", socketPath, "-O", "check", hostAlias)
	if err := check.Run(); err == nil {
		return nil
	}
	args := managedSSHForwardArgs(hostAlias, configPath, socketPath, forwards, sshSpec)
	cmd := exec.Command(args[0], args[1:]...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("start managed ssh forward: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func managedSSHForwardArgs(hostAlias, configPath, socketPath string, forwards []config.PortMapping, sshSpec config.SSHSpec) []string {
	args := []string{
		"ssh",
		"-F", configPath,
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
		switch p.EffectiveDirection() {
		case config.PortDirectionReverse:
			args = append(args, "-R", fmt.Sprintf("%d:127.0.0.1:%d", p.Remote, p.Local))
		default:
			args = append(args, "-L", fmt.Sprintf("%d:127.0.0.1:%d", p.Local, p.Remote))
		}
	}
	args = append(args, hostAlias)
	return args
}

func stopManagedSSHForward(hostAlias string) error {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return err
	}
	configPath, err := sshConfigPath()
	if err != nil {
		return err
	}
	cmd := exec.Command("ssh", "-F", configPath, "-S", socketPath, "-O", "exit", hostAlias)
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
	defer func() {
		_ = os.Remove(lockPath)
	}()

	existing, _ := os.ReadFile(configPath)
	updated := mutator(string(existing))
	return os.WriteFile(configPath, []byte(updated), 0o600)
}
