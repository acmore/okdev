package sync

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

type Pair struct {
	Local  string
	Remote string
}

type Client interface {
	CopyToPod(context.Context, string, string, string, string) error
	CopyFromPod(context.Context, string, string, string, string) error
	ExecSh(context.Context, string, string, string) ([]byte, error)
}

func ParsePairs(configured []string, defaultRemote string) ([]Pair, error) {
	if len(configured) == 0 {
		return []Pair{{Local: ".", Remote: defaultRemote}}, nil
	}
	out := make([]Pair, 0, len(configured))
	for _, item := range configured {
		parts := strings.Split(item, ":")
		if len(parts) != 2 {
			return nil, fmt.Errorf("invalid sync path mapping %q, expected local:remote", item)
		}
		out = append(out, Pair{Local: strings.TrimSpace(parts[0]), Remote: strings.TrimSpace(parts[1])})
	}
	return out, nil
}

func RunOnce(mode string, k Client, namespace, pod string, pairs []Pair, excludes []string) error {
	switch mode {
	case "up":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			err := syncUpPath(ctx, k, namespace, pod, p, excludes)
			cancel()
			if err != nil {
				return err
			}
		}
	case "down":
		for _, p := range pairs {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
			err := syncDownPath(ctx, k, namespace, pod, p, excludes)
			cancel()
			if err != nil {
				return err
			}
		}
	case "bi":
		if err := RunOnce("up", k, namespace, pod, pairs, excludes); err != nil {
			return err
		}
		if err := RunOnce("down", k, namespace, pod, pairs, excludes); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unsupported mode %q (supported: up|down|bi)", mode)
	}
	return nil
}

func syncUpPath(ctx context.Context, k Client, namespace, pod string, p Pair, excludes []string) error {
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
	_, err = k.ExecSh(ctx, namespace, pod, fmt.Sprintf("mkdir -p %s && tar -xf %s -C %s && rm -f %s", ShellEscape(p.Remote), remoteTar, ShellEscape(p.Remote), remoteTar))
	return err
}

func syncDownPath(ctx context.Context, k Client, namespace, pod string, p Pair, excludes []string) error {
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
		excludeFlags += fmt.Sprintf(" --exclude=%s", ShellEscape(strings.TrimSpace(ex)))
	}
	if _, err := k.ExecSh(ctx, namespace, pod, fmt.Sprintf("tar -cf %s%s -C %s .", remoteTar, excludeFlags, ShellEscape(p.Remote))); err != nil {
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

func ShellEscape(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}
