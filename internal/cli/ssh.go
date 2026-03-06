package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

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
		Short: "Connect to session pod over SSH (requires sshd in container)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			if err := ensureSessionLock(opts, cfg, ns, sn, cmd.OutOrStdout()); err != nil {
				return err
			}

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
				keyPath = cfg.Spec.SSH.PrivateKeyPath
			}
			if keyPath == "" {
				home, err := os.UserHomeDir()
				if err != nil {
					return err
				}
				keyPath = filepath.Join(home, ".ssh", "okdev_ed25519")
			}

			if setupKey {
				if err := ensureSSHKeyOnPod(opts, ns, podName(sn), keyPath); err != nil {
					return err
				}
			}

			cancelPF, err := startSSHPortForward(opts, ns, podName(sn), localPort, remotePort)
			if err != nil {
				return err
			}
			defer cancelPF()

			sshArgs := []string{
				"-p", fmt.Sprintf("%d", localPort),
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

func ensureSSHKeyOnPod(opts *Options, namespace, pod, keyPath string) error {
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
	_, err = newKubeClient(opts).ExecSh(ctx, namespace, pod, script)
	if err != nil {
		return fmt.Errorf("install ssh key in pod: %w", err)
	}
	return nil
}

func startSSHPortForward(opts *Options, namespace, pod string, localPort, remotePort int) (context.CancelFunc, error) {
	return startManagedPortForward(opts, namespace, pod, []string{fmt.Sprintf("%d:%d", localPort, remotePort)})
}
