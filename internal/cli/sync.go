package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

type syncPair struct {
	Local  string
	Remote string
}

func newSyncCmd(opts *Options) *cobra.Command {
	var mode string

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Sync files between local workspace and session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			pairs, err := syncPairs(cfg.Spec.Sync.Paths, cfg.Spec.Workspace.MountPath)
			if err != nil {
				return err
			}

			k := newKubeClient(opts)
			ctx, cancel := defaultContext()
			defer cancel()
			pod := podName(sn)

			switch mode {
			case "up":
				for _, p := range pairs {
					if err := syncUpPath(ctx, k, ns, pod, p, cfg.Spec.Sync.Exclude); err != nil {
						return err
					}
				}
			case "down":
				for _, p := range pairs {
					if err := syncDownPath(ctx, k, ns, pod, p, cfg.Spec.Sync.Exclude); err != nil {
						return err
					}
				}
			case "bi":
				for _, p := range pairs {
					if err := syncUpPath(ctx, k, ns, pod, p, cfg.Spec.Sync.Exclude); err != nil {
						return err
					}
				}
				for _, p := range pairs {
					if err := syncDownPath(ctx, k, ns, pod, p, cfg.Spec.Sync.Exclude); err != nil {
						return err
					}
				}
			default:
				return fmt.Errorf("unsupported mode %q (supported: up|down|bi)", mode)
			}

			fmt.Fprintf(cmd.OutOrStdout(), "Sync complete (%s) for session %s\n", mode, sn)
			if mode == "bi" {
				fmt.Fprintln(cmd.OutOrStdout(), "Note: bi mode currently performs upload then download in sequence")
			}
			return nil
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	return cmd
}

func syncPairs(configured []string, defaultRemote string) ([]syncPair, error) {
	if len(configured) == 0 {
		return []syncPair{{Local: ".", Remote: defaultRemote}}, nil
	}
	out := make([]syncPair, 0, len(configured))
	for _, item := range configured {
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid sync path mapping %q, expected local:remote", item)
		}
		out = append(out, syncPair{Local: strings.TrimSpace(parts[0]), Remote: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

func syncUpPath(ctx context.Context, k interface {
	CopyToPod(context.Context, string, string, string, string) error
	ExecSh(context.Context, string, string, string) ([]byte, error)
}, namespace, pod string, p syncPair, excludes []string) error {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return err
	}
	st, err := os.Stat(absLocal)
	if err != nil {
		return err
	}

	if !st.IsDir() {
		return k.CopyToPod(ctx, namespace, absLocal, pod, p.Remote)
	}

	tarFile := filepath.Join(os.TempDir(), fmt.Sprintf("okdev-up-%d.tar", time.Now().UnixNano()))
	defer os.Remove(tarFile)

	if err := createTar(absLocal, tarFile, excludes); err != nil {
		return err
	}
	remoteTar := "/tmp/okdev-sync-up.tar"
	if err := k.CopyToPod(ctx, namespace, tarFile, pod, remoteTar); err != nil {
		return err
	}
	_, err = k.ExecSh(ctx, namespace, pod, fmt.Sprintf("mkdir -p %s && tar -xf %s -C %s && rm -f %s", shellEscape(p.Remote), remoteTar, shellEscape(p.Remote), remoteTar))
	return err
}

func syncDownPath(ctx context.Context, k interface {
	CopyFromPod(context.Context, string, string, string, string) error
	ExecSh(context.Context, string, string, string) ([]byte, error)
}, namespace, pod string, p syncPair, excludes []string) error {
	absLocal, err := filepath.Abs(p.Local)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absLocal, 0o755); err != nil {
		return err
	}

	remoteTar := "/tmp/okdev-sync-down.tar"
	localTar := filepath.Join(os.TempDir(), fmt.Sprintf("okdev-down-%d.tar", time.Now().UnixNano()))
	defer os.Remove(localTar)

	excludeFlags := ""
	for _, ex := range excludes {
		if strings.TrimSpace(ex) == "" {
			continue
		}
		excludeFlags += fmt.Sprintf(" --exclude=%s", shellEscape(strings.TrimSpace(ex)))
	}
	if _, err := k.ExecSh(ctx, namespace, pod, fmt.Sprintf("tar -cf %s -C %s .%s", remoteTar, shellEscape(p.Remote), excludeFlags)); err != nil {
		return err
	}
	if err := k.CopyFromPod(ctx, namespace, pod, remoteTar, localTar); err != nil {
		return err
	}
	if _, err := k.ExecSh(ctx, namespace, pod, fmt.Sprintf("rm -f %s", remoteTar)); err != nil {
		return err
	}
	return extractTar(localTar, absLocal)
}

func createTar(srcDir, outTar string, excludes []string) error {
	args := []string{"-cf", outTar}
	for _, ex := range excludes {
		ex := strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		args = append(args, "--exclude", ex)
	}
	args = append(args, "-C", srcDir, ".")
	cmd := exec.Command("tar", args...)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("create tar: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func extractTar(tarFile, destDir string) error {
	cmd := exec.Command("tar", "-xf", tarFile, "-C", destDir)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("extract tar: %w (%s)", err, strings.TrimSpace(string(out)))
	}
	return nil
}

func shellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
