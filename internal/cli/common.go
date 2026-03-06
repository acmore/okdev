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
	"regexp"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

const sessionHeartbeatInterval = 5 * time.Minute

var invalidOwnerChars = regexp.MustCompile(`[^a-z0-9._-]`)

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

func labelsForSession(opts *Options, cfg *config.DevEnvironment, sessionName string) map[string]string {
	owner := currentOwner(opts)
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
		"okdev.io/shareable": func() string {
			if cfg.Spec.Session.Shareable {
				return "true"
			}
			return "false"
		}(),
	}
}

func annotationsForSession(cfg *config.DevEnvironment) map[string]string {
	out := map[string]string{
		"okdev.io/last-attach": time.Now().UTC().Format(time.RFC3339),
		"okdev.io/shareable": func() string {
			if cfg.Spec.Session.Shareable {
				return "true"
			}
			return "false"
		}(),
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
	_ = cfg
	_ = renewLock
	if !emitHeartbeat {
		return func() {}
	}
	pod := podName(sessionName)
	ctx, cancel := context.WithCancel(context.Background())

	go func() {
		heartbeatTicker := time.NewTicker(sessionHeartbeatInterval)
		defer heartbeatTicker.Stop()

		doHeartbeat := func() {
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
			case <-heartbeatTicker.C:
				doHeartbeat()
			}
		}
	}()

	return cancel
}

func currentOwner(opts *Options) string {
	if opts != nil {
		if v := normalizeOwner(opts.Owner); v != "" {
			return v
		}
	}
	if v := normalizeOwner(os.Getenv("OKDEV_OWNER")); v != "" {
		return v
	}
	if v := normalizeOwner(os.Getenv("USER")); v != "" {
		return v
	}
	return "dev"
}

func normalizeOwner(v string) string {
	s := strings.ToLower(strings.TrimSpace(v))
	s = strings.ReplaceAll(s, " ", "-")
	s = invalidOwnerChars.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	return s
}

func ownerLabelSelector(opts *Options) string {
	return "okdev.io/owner=" + currentOwner(opts)
}

func isSessionShareable(p kube.PodSummary) bool {
	if strings.EqualFold(strings.TrimSpace(p.Annotations["okdev.io/shareable"]), "true") {
		return true
	}
	return strings.EqualFold(strings.TrimSpace(p.Labels["okdev.io/shareable"]), "true")
}

func ensureSessionOwnership(opts *Options, k *kube.Client, namespace, sessionName string, allowShareable bool) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	pods, err := k.ListPods(ctx, namespace, false, "okdev.io/managed=true,okdev.io/session="+sessionName)
	if err != nil {
		return err
	}
	if len(pods) == 0 {
		return nil
	}
	owner := currentOwner(opts)
	otherOwner := strings.TrimSpace(pods[0].Labels["okdev.io/owner"])
	if otherOwner == "" || otherOwner == owner {
		return nil
	}
	if allowShareable && isSessionShareable(pods[0]) {
		return nil
	}
	return fmt.Errorf("session %q is owned by %q (current owner: %q); set --owner %s or mark session as shareable", sessionName, otherOwner, owner, otherOwner)
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
