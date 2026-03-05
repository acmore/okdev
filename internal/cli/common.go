package cli

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

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
	return map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/name":    cfg.Metadata.Name,
		"okdev.io/session": sessionName,
	}
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
	return newKubeClient(opts).ExecInteractive(ctx, namespace, podName(sessionName), tty, command, os.Stdin, os.Stdout, os.Stderr)
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
