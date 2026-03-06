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
const sessionHeartbeatInterval = 5 * time.Minute

func loadConfigAndNamespace(opts *Options) (*config.DevEnvironment, string, error) {
	cfg, path, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	announceConfigPath(path)
	ns := cfg.Spec.Namespace
	if opts.Namespace != "" {
		ns = opts.Namespace
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func announceConfigPath(path string) {
	if strings.TrimSpace(path) == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "Using config: %s\n", path)
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
	return runConnectWithClient(newKubeClient(opts), namespace, sessionName, command, tty)
}

func runConnectWithClient(k *kube.Client, namespace, sessionName string, command []string, tty bool) error {
	ctx, cancel := interactiveContext()
	defer cancel()
	slog.Debug("connect start", "namespace", namespace, "session", sessionName, "tty", tty)
	return connect.Run(ctx, k, namespace, podName(sessionName), command, tty, os.Stdin, os.Stdout, os.Stderr)
}

func ensureSessionLock(opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer) error {
	stopRenew, err := acquireSessionLockWithClient(newKubeClient(opts), cfg, namespace, sessionName, out)
	if err != nil {
		return err
	}
	stopRenew()
	return nil
}

func acquireSessionLock(opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer) (func(), error) {
	return acquireSessionLockWithClient(newKubeClient(opts), cfg, namespace, sessionName, out)
}

func acquireSessionLockWithClient(k *kube.Client, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer) (func(), error) {
	if cfg.Spec.Session.LockMode == "none" {
		return func() {}, nil
	}
	holder := sessionHolderIdentity()
	leaseName := "okdev-" + sessionName
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	res, err := k.AcquireLease(ctx, namespace, leaseName, holder, cfg.Spec.Session.LockMode, sessionLeaseDuration)
	if err != nil {
		return nil, err
	}
	if !res.Acquired && cfg.Spec.Session.LockMode == "advisory" {
		fmt.Fprintf(out, "warning: session lock currently held by %s\n", res.CurrentHolder)
	}
	return func() {}, nil
}

func startSessionHeartbeat(opts *Options, namespace, sessionName string, out io.Writer, interval time.Duration) func() {
	return startSessionHeartbeatWithClient(newKubeClient(opts), namespace, sessionName, out, interval)
}

func startSessionHeartbeatWithClient(k *kube.Client, namespace, sessionName string, out io.Writer, interval time.Duration) func() {
	if interval <= 0 {
		interval = time.Minute
	}
	pod := podName(sessionName)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		ticker := time.NewTicker(interval)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				beatCtx, beatCancel := context.WithTimeout(context.Background(), 10*time.Second)
				err := k.TouchPodActivity(beatCtx, namespace, pod)
				beatCancel()
				if err != nil {
					slog.Warn("session activity heartbeat failed", "namespace", namespace, "session", sessionName, "pod", pod, "error", err)
					fmt.Fprintf(out, "warning: failed to update session activity heartbeat: %v\n", err)
				}
			}
		}
	}()
	return cancel
}

func startSessionMaintenance(opts *Options, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer, renewLock bool, emitHeartbeat bool) func() {
	return startSessionMaintenanceWithClient(newKubeClient(opts), cfg, namespace, sessionName, out, renewLock, emitHeartbeat)
}

func startSessionMaintenanceWithClient(k *kube.Client, cfg *config.DevEnvironment, namespace, sessionName string, out io.Writer, renewLock bool, emitHeartbeat bool) func() {
	if !renewLock && !emitHeartbeat {
		return func() {}
	}
	holder := sessionHolderIdentity()
	leaseName := "okdev-" + sessionName
	pod := podName(sessionName)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		var renewTicker *time.Ticker
		var renewCh <-chan time.Time
		if renewLock && cfg.Spec.Session.LockMode != "none" {
			renewTicker = time.NewTicker(sessionLeaseDuration / 2)
			renewCh = renewTicker.C
			defer renewTicker.Stop()
		}
		var heartbeatTicker *time.Ticker
		var heartbeatCh <-chan time.Time
		if emitHeartbeat {
			heartbeatTicker = time.NewTicker(sessionHeartbeatInterval)
			heartbeatCh = heartbeatTicker.C
			defer heartbeatTicker.Stop()
		}

		doRenew := func() {
			if !renewLock || cfg.Spec.Session.LockMode == "none" {
				return
			}
			renewCtx, renewCancel := context.WithTimeout(context.Background(), 15*time.Second)
			renewRes, renewErr := k.AcquireLease(renewCtx, namespace, leaseName, holder, cfg.Spec.Session.LockMode, sessionLeaseDuration)
			renewCancel()
			if renewErr != nil {
				slog.Warn("session lock renewal failed", "namespace", namespace, "session", sessionName, "holder", holder, "error", renewErr)
				fmt.Fprintf(out, "warning: failed to renew session lock: %v\n", renewErr)
				return
			}
			if !renewRes.Acquired && cfg.Spec.Session.LockMode == "advisory" {
				slog.Warn("session advisory lock not acquired on renewal", "namespace", namespace, "session", sessionName, "holder", holder, "current_holder", renewRes.CurrentHolder)
				fmt.Fprintf(out, "warning: session lock now held by %s\n", renewRes.CurrentHolder)
			}
		}

		doHeartbeat := func() {
			if !emitHeartbeat {
				return
			}
			beatCtx, beatCancel := context.WithTimeout(context.Background(), 10*time.Second)
			err := k.TouchPodActivity(beatCtx, namespace, pod)
			beatCancel()
			if err != nil {
				slog.Warn("session activity heartbeat failed", "namespace", namespace, "session", sessionName, "pod", pod, "error", err)
				fmt.Fprintf(out, "warning: failed to update session activity heartbeat: %v\n", err)
			}
		}

		doHeartbeat()
		for {
			select {
			case <-ctx.Done():
				return
			case <-renewCh:
				doRenew()
			case <-heartbeatCh:
				doHeartbeat()
			}
		}
	}()

	return cancel
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
