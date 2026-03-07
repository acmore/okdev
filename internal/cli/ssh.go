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
	"time"

	"github.com/acmore/okdev/internal/config"
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
				localPort = cfg.Spec.SSH.LocalPort
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

			cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(newKubeClient(opts), ns, podName(sn), localPort, remotePort)
			if err != nil {
				return err
			}
			defer cancelPF()

			sshHost := sshHostAlias(sn)
			cfgPath, _ := config.ResolvePath(opts.ConfigPath)
			if cfgErr := ensureSSHConfigEntry(sshHost, sn, user, localPort, remotePort, keyPath, cfgPath); cfgErr != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to update ~/.ssh/config: %v\n", cfgErr)
			}

			sshArgs := []string{
				"-F", "/dev/null",
				"-p", fmt.Sprintf("%d", usedLocalPort),
				"-o", "StrictHostKeyChecking=no",
				"-o", "UserKnownHostsFile=/dev/null",
				"-i", keyPath,
			}
			if cmdStr != "" {
				sshArgs = append(sshArgs, fmt.Sprintf("%s@127.0.0.1", user), cmdStr)
			} else {
				sshArgs = append(sshArgs, fmt.Sprintf("%s@127.0.0.1", user))
			}
			sshCmd := exec.Command("ssh", sshArgs...)
			sshCmd.Stdin = os.Stdin
			sshCmd.Stdout = os.Stdout
			sshCmd.Stderr = os.Stderr
			if err := sshCmd.Run(); err != nil {
				return fmt.Errorf("ssh failed: %w", err)
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
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	k := newKubeClient(opts)
	container := sshTargetContainer(cfg)
	if container == "" {
		_, err = k.ExecSh(ctx, namespace, pod, script)
	} else {
		_, err = k.ExecShInContainer(ctx, namespace, pod, container, script)
	}
	if err != nil {
		return fmt.Errorf("install ssh key in pod: %w", err)
	}
	return nil
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
	return filepath.Join(home, ".ssh", "okdev_ed25519"), nil
}

func sshTargetContainer(cfg *config.DevEnvironment) string {
	if cfg == nil {
		return ""
	}
	if strings.EqualFold(strings.TrimSpace(cfg.Spec.SSH.Mode), "sidecar") {
		return "okdev-ssh"
	}
	return ""
}

func sshHostAlias(sessionName string) string {
	return "okdev-" + sessionName
}

func ensureSSHConfigEntry(hostAlias, sessionName, user string, localPort, remotePort int, keyPath, okdevConfigPath string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	sshDir := filepath.Join(home, ".ssh")
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		return err
	}
	sshConfigPath := filepath.Join(sshDir, "config")
	existing, _ := os.ReadFile(sshConfigPath)

	begin := "# BEGIN OKDEV " + hostAlias
	end := "# END OKDEV " + hostAlias
	proxyInner := fmt.Sprintf("okdev --session %s ssh-proxy --local-port %d --remote-port %d", shellQuote(sessionName), localPort, remotePort)
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
		end,
	}
	block := strings.Join(blockLines, "\n") + "\n"
	updated := stripManagedSSHBlock(string(existing), begin, end) + "\n" + block
	updated = strings.TrimSpace(updated) + "\n"
	return os.WriteFile(sshConfigPath, []byte(updated), 0o600)
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
	var localPort int
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
			if localPort == 0 {
				localPort = cfg.Spec.SSH.LocalPort
			}
			if remotePort == 0 {
				remotePort = cfg.Spec.SSH.RemotePort
			}
			cancelPF, usedLocalPort, err := startSSHPortForwardWithFallback(k, ns, podName(sn), localPort, remotePort)
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
			setErr := func(err error) {
				if err == nil || errors.Is(err, io.EOF) {
					return
				}
				once.Do(func() { copyErr = err })
			}
			wg.Add(2)
			go func() {
				defer wg.Done()
				_, err := io.Copy(conn, os.Stdin)
				setErr(err)
				if cw, ok := conn.(interface{ CloseWrite() error }); ok {
					_ = cw.CloseWrite()
				}
			}()
			go func() {
				defer wg.Done()
				_, err := io.Copy(os.Stdout, conn)
				setErr(err)
			}()
			wg.Wait()
			return copyErr
		},
	}
	cmd.Flags().IntVar(&localPort, "local-port", 0, "Local port for SSH tunnel")
	cmd.Flags().IntVar(&remotePort, "remote-port", 0, "Remote SSH port in pod")
	return cmd
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
