package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

const sessionLeaseDuration = 2 * time.Minute

func loadConfigAndNamespace(opts *Options) (*config.DevEnvironment, string, error) {
	cfg, _, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	ns := cfg.Spec.Namespace
	if opts.Namespace != "" {
		ns = opts.Namespace
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func resolveSessionName(opts *Options, cfg *config.DevEnvironment) (string, error) {
	return session.Resolve(opts.Session, cfg.Spec.Session.DefaultNameTemplate)
}

func labelsForSession(cfg *config.DevEnvironment, sessionName string) map[string]string {
	owner := os.Getenv("USER")
	if owner == "" {
		owner = "dev"
	}
	repo := "unknown"
	if root, err := session.RepoRoot(); err == nil && root != "" {
		repo = filepath.Base(root)
	}
	return map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/name":    cfg.Metadata.Name,
		"okdev.io/session": sessionName,
		"okdev.io/owner":   owner,
		"okdev.io/repo":    repo,
	}
}

func annotationsForSession(cfg *config.DevEnvironment) map[string]string {
	out := map[string]string{
		"okdev.io/last-attach": time.Now().UTC().Format(time.RFC3339),
	}
	if cfg.Spec.Session.TTLHours > 0 {
		out["okdev.io/ttl-hours"] = fmt.Sprintf("%d", cfg.Spec.Session.TTLHours)
	}
	if cfg.Spec.Session.IdleTimeoutMinutes > 0 {
		out["okdev.io/idle-timeout-minutes"] = fmt.Sprintf("%d", cfg.Spec.Session.IdleTimeoutMinutes)
	}
	return out
}

func podName(sessionName string) string {
	return "okdev-" + sessionName
}

func pvcName(cfg *config.DevEnvironment, sessionName string) string {
	if cfg.Spec.Workspace.PVC.ClaimName != "" {
		return cfg.Spec.Workspace.PVC.ClaimName
	}
	return "okdev-" + sessionName + "-workspace"
}

func newKubeClient(opts *Options) *kube.Client {
	return &kube.Client{Context: opts.Context}
}

func defaultContext() (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func interactiveContext() (context.Context, context.CancelFunc) {
	return context.WithCancel(context.Background())
}

func runConnect(opts *Options, namespace, sessionName string, command []string, tty bool) error {
	ctx, cancel := interactiveContext()
	defer cancel()
	slog.Debug("connect start", "namespace", namespace, "session", sessionName, "tty", tty)
	return connect.Run(ctx, newKubeClient(opts), namespace, podName(sessionName), command, tty, os.Stdin, os.Stdout, os.Stderr)
}

func ensureSessionLock(opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer) error {
	stopRenew, err := acquireSessionLock(opts, cfg, namespace, sessionName, out, false)
	if err != nil {
		return err
	}
	stopRenew()
	return nil
}

func acquireSessionLock(opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer, renew bool) (func(), error) {
	if cfg.Spec.Session.LockMode == "none" {
		return func() {}, nil
	}
	holder := sessionHolderIdentity()
	leaseName := "okdev-" + sessionName
	k := newKubeClient(opts)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := k.AcquireLease(ctx, namespace, leaseName, holder, cfg.Spec.Session.LockMode, sessionLeaseDuration)
	if err != nil {
		return nil, err
	}
	if !res.Acquired && cfg.Spec.Session.LockMode == "advisory" {
		fmt.Fprintf(out, "warning: session lock currently held by %s\n", res.CurrentHolder)
	}
	if !renew {
		return func() {}, nil
	}

	renewCtx, renewCancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(sessionLeaseDuration / 2)
		defer ticker.Stop()
		for {
			select {
			case <-renewCtx.Done():
				return
			case <-ticker.C:
				renewCallCtx, renewCallCancel := context.WithTimeout(context.Background(), 15*time.Second)
				renewRes, renewErr := k.AcquireLease(renewCallCtx, namespace, leaseName, holder, cfg.Spec.Session.LockMode, sessionLeaseDuration)
				renewCallCancel()
				if renewErr != nil {
					fmt.Fprintf(out, "warning: failed to renew session lock: %v\n", renewErr)
					continue
				}
				if !renewRes.Acquired && cfg.Spec.Session.LockMode == "advisory" {
					fmt.Fprintf(out, "warning: session lock now held by %s\n", renewRes.CurrentHolder)
				}
			}
		}
	}()

	return renewCancel, nil
}

func sessionHolderIdentity() string {
	user := os.Getenv("USER")
	if user == "" {
		user = "dev"
	}
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown-host"
	}
	return user + "@" + host
}

func ensureCommand(name string) error {
	if _, err := exec.LookPath(name); err != nil {
		return fmt.Errorf("required command %q not found in PATH", name)
	}
	return nil
}

func outputJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func age(ts time.Time) string {
	if ts.IsZero() {
		return "-"
	}
	d := time.Since(ts)
	if d < time.Minute {
		return fmt.Sprintf("%ds", int(d.Seconds()))
	}
	if d < time.Hour {
		return fmt.Sprintf("%dm", int(d.Minutes()))
	}
	if d < 24*time.Hour {
		return fmt.Sprintf("%dh", int(d.Hours()))
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
