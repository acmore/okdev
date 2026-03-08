package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
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
			stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), true, true)
			defer stopMaintenance()

			if user == "" {
				user = cfg.Spec.SSH.User
			}
			if remotePort == 0 {
				remotePort = cfg.Spec.SSH.RemotePort
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
				if err := ensureSSHKeyOnPod(opts, cfg, ns, podName(sn), keyPath); err != nil {
					return err
				}
			}
			if err := waitForSSHDReady(opts, cfg, ns, podName(sn), 20*time.Second); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: sshd not ready yet: %v\n", err)
			}

			cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(newKubeClient(opts), ns, podName(sn), localPort, remotePort)
			if err != nil {
				return err
			}
			var pfMu sync.Mutex
			currentCancelPF := cancelPF
			defer func() {
				pfMu.Lock()
				defer pfMu.Unlock()
				if currentCancelPF != nil {
					currentCancelPF()
				}
			}()

			sshHost := sshHostAlias(sn)
			cfgPath, _ := config.ResolvePath(opts.ConfigPath)
			if cfgErr := ensureSSHConfigEntry(sshHost, sn, ns, user, remotePort, keyPath, cfgPath, cfg.Spec.Ports); cfgErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
			}

			tm := &okssh.TunnelManager{
				SSHUser:           user,
				SSHKeyPath:        keyPath,
				RemotePort:        remotePort,
				KeepAliveInterval: time.Duration(cfg.Spec.SSH.KeepAliveInterval) * time.Second,
				KeepAliveTimeout:  time.Duration(cfg.Spec.SSH.KeepAliveTimeout) * time.Second,
			}
			if noTmux {
				tm.Env = map[string]string{"OKDEV_NO_TMUX": "1"}
			}
			var hadReconnect atomic.Bool
			tm.SetConnectionStateCallback(func(state okssh.ConnectionState) {
				switch state {
				case okssh.StateReconnecting:
					hadReconnect.Store(true)
					fmt.Fprintln(cmd.ErrOrStderr(), "SSH connection lost, reconnecting...")
				case okssh.StateConnected:
					if hadReconnect.Swap(false) {
						fmt.Fprintln(cmd.ErrOrStderr(), "SSH connection restored.")
					}
				}
			})
			tm.SetReconnectTargetProvider(func(_ context.Context) (string, int, error) {
				pfMu.Lock()
				defer pfMu.Unlock()
				if currentCancelPF != nil {
					currentCancelPF()
					currentCancelPF = nil
				}
				cancel, lp, err := startSSHPortForwardWithFallback(newKubeClient(opts), ns, podName(sn), localPort, remotePort)
				if err != nil {
					return "", 0, err
				}
				currentCancelPF = cancel
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
			if err := tm.Connect(cmd.Context(), "127.0.0.1", usedLocalPort); err != nil {
				return fmt.Errorf("connect ssh tunnel: %w", err)
			}
			defer tm.Close()

			for _, p := range cfg.Spec.Ports {
				if p.Local <= 0 || p.Remote <= 0 {
					continue
				}
				if err := tm.AddForward(p.Local, p.Remote); err != nil {
					if errors.Is(err, okssh.ErrLocalPortInUse) {
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: local port %d already in use; skip forward %d->%d\n", p.Local, p.Local, p.Remote)
						continue
					}
					fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to add forward %d->%d: %v\n", p.Local, p.Remote, err)
				}
			}
			if cfg.Spec.SSH.AutoDetectPorts == nil || *cfg.Spec.SSH.AutoDetectPorts {
				configured := map[int]struct{}{}
				for _, p := range cfg.Spec.Ports {
					if p.Remote > 0 {
						configured[p.Remote] = struct{}{}
					}
				}
				exclude := map[int]struct{}{
					cfg.Spec.SSH.RemotePort: {},
					8384:                    {}, // syncthing gui
					22000:                   {}, // syncthing sync
				}
				tm.StartAutoDetect(5*time.Second, configured, exclude, func(port int) {
					if err := tm.AddForward(port, port); err != nil {
						if errors.Is(err, okssh.ErrLocalPortInUse) {
							fmt.Fprintf(cmd.ErrOrStderr(), "warning: detected remote port %d but local %d is already in use; skipping auto-forward\n", port, port)
							return
						}
						fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to auto-forward port %d: %v\n", port, err)
					}
				}, func(port int) {
					tm.RemoveForward(port)
				})
			}

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
			if err := tm.OpenShell(); err != nil {
				if !tm.IsConnected() || isIgnorableProxyIOError(err) {
					fmt.Fprintln(cmd.ErrOrStderr(), "Connection lost. Session ended.")
					return nil
				}
				return fmt.Errorf("ssh shell failed: %w", err)
			}
			return nil
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

func ensureSSHKeyOnPod(opts *Options, cfg *config.DevEnvironment, namespace, pod, keyPath string) error {
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
	for i := 0; i < 3; i++ {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		if container == "" {
			_, err = k.ExecSh(ctx, namespace, pod, script)
		} else {
			_, err = k.ExecShInContainer(ctx, namespace, pod, container, script)
		}
		cancel()
		if err == nil {
			return nil
		}
		lastErr = err
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}
	return fmt.Errorf("install ssh key in pod: %w", lastErr)
}

func waitForSSHDReady(opts *Options, cfg *config.DevEnvironment, namespace, pod string, timeout time.Duration) error {
	k := newKubeClient(opts)
	container := sshTargetContainer(cfg)
	if container == "" {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for time.Now().Before(deadline) {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_, err := k.ExecShInContainer(ctx, namespace, pod, container, "ps | grep '[s]shd' >/dev/null 2>&1")
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

func ensureSSHConfigEntry(hostAlias, sessionName, namespace, user string, remotePort int, keyPath, okdevConfigPath string, forwards []config.PortMapping) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
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
	for _, p := range forwards {
		if p.Local <= 0 || p.Remote <= 0 {
			continue
		}
		blockLines = append(blockLines, fmt.Sprintf("  LocalForward %d 127.0.0.1:%d", p.Local, p.Remote))
	}
	blockLines = append(blockLines,
		"  ServerAliveInterval 30",
		"  ServerAliveCountMax 10",
		"  TCPKeepAlive yes",
		"  LogLevel ERROR",
		end,
	)
	block := strings.Join(blockLines, "\n") + "\n"
	return updateSSHConfigWithLock(sshConfigPath, func(existing string) string {
		updated := stripManagedSSHBlock(existing, begin, end) + "\n" + block
		return strings.TrimSpace(updated) + "\n"
	})
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
			conn, err := waitDialLocal(usedLocalPort, 10*time.Second)
			if err != nil {
				return err
			}
			defer conn.Close()
			var wg sync.WaitGroup
			var copyErr error
			var once sync.Once
			done := make(chan struct{})
			setErr := func(err error) {
				if err == nil || errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) || errors.Is(err, syscall.EPIPE) || isIgnorableProxyIOError(err) {
					return
				}
				once.Do(func() { copyErr = err })
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := io.Copy(conn, os.Stdin)
				setErr(err)
				select {
				case <-done:
				default:
					_ = conn.Close()
				}
			}()
			go func() {
				defer wg.Done()
				_, err := io.Copy(os.Stdout, conn)
				setErr(err)
				close(done)
				_ = conn.Close()
			}()
			<-done
			return copyErr
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
		strings.Contains(msg, "connection reset by peer")
}

func waitDialLocal(localPort int, timeout time.Duration) (net.Conn, error) {
	deadline := time.Now().Add(timeout)
	addr := fmt.Sprintf("127.0.0.1:%d", localPort)
	var lastErr error
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 750*time.Millisecond)
		if err == nil {
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

func startManagedSSHForward(hostAlias string) error {
	socketPath, err := sshControlSocketPath(hostAlias)
	if err != nil {
		return err
	}
	check := exec.Command("ssh", "-S", socketPath, "-O", "check", hostAlias)
	if err := check.Run(); err == nil {
		return nil
	}
	cmd := exec.Command(
		"ssh",
		"-fN",
		"-M",
		"-S", socketPath,
		"-o", "ControlPersist=600",
		"-o", "ExitOnForwardFailure=no",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=10",
		"-o", "TCPKeepAlive=yes",
		"-o", "LogLevel=ERROR",
		hostAlias,
	)
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
		cancel, err := startManagedPortForwardNoProbeWithClient(k, namespace, pod, []string{fmt.Sprintf("%d:%d", lp, remotePort)})
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
