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
	"sort"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

const sessionHeartbeatInterval = 5 * time.Minute

var invalidOwnerChars = regexp.MustCompile(`[^a-z0-9._-]`)

func loadConfigAndNamespace(opts *Options) (*config.DevEnvironment, string, error) {
	cfg, path, err := config.Load(opts.ConfigPath)
	if err != nil {
		return nil, "", err
	}
	announceConfigPath(path)
	applyConfigKubeContext(opts, cfg)
	ns := cfg.Spec.Namespace
	if opts.Namespace != "" {
		ns = opts.Namespace
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}
	return cfg, ns, nil
}

func applyConfigKubeContext(opts *Options, cfg *config.DevEnvironment) {
	if opts == nil || cfg == nil {
		return
	}
	if strings.TrimSpace(opts.Context) != "" {
		return
	}
	if ctx := strings.TrimSpace(cfg.Spec.KubeContext); ctx != "" {
		opts.Context = ctx
	}
}

func announceConfigPath(path string) {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("OKDEV_QUIET_CONFIG_ANNOUNCE")), "1") {
		return
	}
	if strings.TrimSpace(path) == "" {
		return
	}
	fmt.Fprintf(os.Stderr, "Using config: %s\n", path)
}

func resolveSessionName(opts *Options, cfg *config.DevEnvironment) (string, error) {
	return session.Resolve(opts.Session, cfg.Spec.Session.DefaultNameTemplate)
}

func resolveSessionNameForUpDown(opts *Options, cfg *config.DevEnvironment, namespace string) (string, error) {
	if strings.TrimSpace(opts.Session) != "" {
		return resolveSessionName(opts, cfg)
	}
	active, err := session.LoadActiveSession()
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(active) != "" {
		return session.Resolve(active, cfg.Spec.Session.DefaultNameTemplate)
	}
	inferred, err := inferExistingSessionForRepo(opts, cfg, namespace)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(inferred) != "" {
		return inferred, nil
	}
	return session.Resolve("", cfg.Spec.Session.DefaultNameTemplate)
}

func inferExistingSessionForRepo(opts *Options, cfg *config.DevEnvironment, namespace string) (string, error) {
	root, err := session.RepoRoot()
	if err != nil || strings.TrimSpace(root) == "" {
		return "", nil
	}
	repo := filepath.Base(root)
	if strings.TrimSpace(repo) == "" {
		return "", nil
	}
	label := []string{
		"okdev.io/managed=true",
		ownerLabelSelector(opts),
		"okdev.io/repo=" + repo,
	}
	if strings.TrimSpace(cfg.Metadata.Name) != "" {
		label = append(label, "okdev.io/name="+cfg.Metadata.Name)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	pods, err := newKubeClient(opts).ListPods(ctx, namespace, false, strings.Join(label, ","))
	if err != nil {
		return "", err
	}
	if len(pods) == 0 {
		return "", nil
	}
	sort.Slice(pods, func(i, j int) bool {
		return pods[i].CreatedAt.After(pods[j].CreatedAt)
	})
	for _, p := range pods {
		sn := strings.TrimSpace(p.Labels["okdev.io/session"])
		if sn != "" {
			return sn, nil
		}
		if strings.HasPrefix(p.Name, "okdev-") {
			return strings.TrimPrefix(p.Name, "okdev-"), nil
		}
	}
	return "", nil
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

func usesWorkspacePVC(cfg *config.DevEnvironment) bool {
	if cfg == nil {
		return false
	}
	pvc := cfg.Spec.Workspace.PVC
	return strings.TrimSpace(pvc.ClaimName) != "" ||
		strings.TrimSpace(pvc.Size) != "" ||
		strings.TrimSpace(pvc.StorageClassName) != ""
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
	pod, err := k.GetPodSummary(ctx, namespace, podName(sessionName))
	if err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return err
	}
	owner := currentOwner(opts)
	otherOwner := strings.TrimSpace(pod.Labels["okdev.io/owner"])
	if otherOwner == "" || otherOwner == owner {
		return nil
	}
	if allowShareable && isSessionShareable(*pod) {
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
