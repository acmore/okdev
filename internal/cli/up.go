package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

func newUpCmd(opts *Options) *cobra.Command {
	var waitTimeout time.Duration
	var dryRun bool
	var tmux bool
	var sshMode string

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a dev session",
		RunE: func(cmd *cobra.Command, args []string) error {
			if sshMode != "sidecar" && sshMode != "embedded" {
				return fmt.Errorf("invalid --ssh-mode %q: must be \"sidecar\" or \"embedded\"", sshMode)
			}
			ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
			// Keep legacy config-path announce quiet for this command; we render it in staged output.
			prevQuiet, hadQuiet := os.LookupEnv("OKDEV_QUIET_CONFIG_ANNOUNCE")
			_ = os.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", "1")
			defer func() {
				if hadQuiet {
					_ = os.Setenv("OKDEV_QUIET_CONFIG_ANNOUNCE", prevQuiet)
				} else {
					_ = os.Unsetenv("OKDEV_QUIET_CONFIG_ANNOUNCE")
				}
			}()

			ui.section("Validate")
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			cfgPath, pathErr := config.ResolvePath(opts.ConfigPath)
			if pathErr != nil {
				return pathErr
			}
			ui.stepDone("config", cfgPath)
			k := newKubeClient(opts)
			sn, err := resolveSessionNameForUpDown(opts, cfg, ns)
			if err != nil {
				return err
			}
			ui.stepDone("session", sn)
			ui.stepDone("namespace", ns)
			labels := labelsForSession(opts, cfg, sn)
			annotations := annotationsForSession(cfg)
			volumes := cfg.EffectiveVolumes()
			pod := podName(sn)
			if dryRun {
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", sn, ns)
				fmt.Fprintf(cmd.OutOrStdout(), "- using %d configured volume(s)\n", len(volumes))
				fmt.Fprintf(cmd.OutOrStdout(), "- would apply pod/%s\n", pod)
				fmt.Fprintf(cmd.OutOrStdout(), "- would wait for pod readiness (timeout=%s)\n", waitTimeout)
				fmt.Fprintln(cmd.OutOrStdout(), "- would setup SSH config + managed SSH/port-forwards")
				fmt.Fprintln(cmd.OutOrStdout(), "- would start background sync (when sync.engine=syncthing)")
				return nil
			}

			ui.section("Reconcile")
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			ui.stepDone("ownership", "ok")
			if warnErr := warnIfConfigNewerThanSession(opts, k, ns, sn, pod, ui.warnWriter()); warnErr != nil {
				slog.Debug("skip config drift warning", "error", warnErr)
			}
			ctx, cancel := defaultContext()
			defer cancel()
			ui.stepDone("pvc", "not managed (use pre-created PVCs in spec.volumes)")

			enableTmux := tmux || cfg.Spec.SSH.PersistentSessionEnabled()
			preStopCmd := resolvePreStopCommand(cfg, cfgPath)
			preparedSpec, err := kube.PreparePodSpec(
				cfg.Spec.PodTemplate.Spec,
				volumes,
				cfg.WorkspaceMountPath(),
				cfg.Spec.Sidecar.Image,
				enableTmux,
				sshMode == "embedded",
				preStopCmd,
			)
			if err != nil {
				return err
			}

			podManifest, err := kube.BuildPodManifest(ns, pod, labels, annotations, preparedSpec)
			if err != nil {
				return err
			}
			ui.stepRun("pod", pod)
			if err := k.Apply(ctx, ns, podManifest); err != nil {
				return err
			}
			ui.stepDone("pod", "applied")
			ui.section("Wait")
			progressPrinter := func(p kube.PodReadinessProgress) {
				reason := strings.TrimSpace(p.Reason)
				if reason == "" {
					reason = "-"
				}
				ui.stepRun("pod readiness", fmt.Sprintf("%s %d/%d (%s)", p.Phase, p.ReadyContainers, p.TotalContainers, reason))
			}
			if err := k.WaitReadyWithProgress(ctx, ns, pod, waitTimeout, progressPrinter); err != nil {
				hints := fmt.Sprintf("next steps:\n- run `okdev status --session %s`\n- run `kubectl -n %s describe pod %s`", sn, ns, pod)
				diag, derr := k.DescribePod(ctx, ns, pod)
				if derr == nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "pod diagnostics:\n%s\n\n%s\n", diag, hints)
					return fmt.Errorf("wait for pod/%s readiness failed: %w", pod, err)
				}
				fmt.Fprintln(cmd.ErrOrStderr(), hints)
				return fmt.Errorf("wait for pod/%s readiness failed: %w", pod, err)
			}
			ui.stepDone("pod readiness", "ready")

			if err := session.SaveActiveSession(sn); err != nil {
				return err
			}
			ui.stepDone("active session", sn)

			ui.section("Setup")
			sshSummary := "disabled"
			if cfg.Spec.SSH.RemotePort > 0 {
				keyPath, keyErr := defaultSSHKeyPath(cfg)
				if keyErr != nil {
					ui.warnf("failed to resolve SSH key path: %v", keyErr)
					sshSummary = "degraded (key path error)"
				} else {
					if err := ensureSSHKeyOnPod(opts, cfg, ns, pod, keyPath); err != nil {
						ui.warnf("failed to setup SSH key in pod: %v", err)
						sshSummary = "degraded (key setup failed)"
					}
					if err := waitForSSHDReady(opts, cfg, ns, pod, 20*time.Second); err != nil {
						ui.warnf("sshd not ready yet: %v", err)
						sshSummary = "degraded (sshd not ready)"
					}
					alias := sshHostAlias(sn)
					if _, cfgErr := ensureSSHConfigEntry(alias, sn, ns, cfg.Spec.SSH.User, cfg.Spec.SSH.RemotePort, keyPath, cfgPath, cfg.Spec.Ports, cfg.Spec.SSH); cfgErr != nil {
						ui.warnf("failed to update ~/.ssh/config: %v", cfgErr)
						sshSummary = "degraded (ssh config update failed)"
					} else {
						// Force-refresh managed master so forward rules are always current after `okdev up`.
						_ = stopManagedSSHForward(alias)
						if err := startManagedSSHForwardWithForwards(alias, cfg.Spec.Ports, cfg.Spec.SSH); err != nil {
							ui.warnf("failed to start managed SSH/port-forwards: %v", err)
							sshSummary = "degraded (managed forward failed)"
						} else {
							sshSummary = "ssh " + alias
							ui.stepDone("ssh", sshSummary)
						}
					}
				}
			}

			syncSummary := "disabled"
			if cfg.Spec.Sync.Engine == "" || cfg.Spec.Sync.Engine == "syncthing" {
				logPath, started, err := startDetachedSyncthingSync(opts, "bi", sn, ns, cfgPath)
				if err != nil {
					ui.warnf("failed to start syncthing background sync: %v", err)
					syncSummary = "degraded (start failed)"
				} else {
					if started {
						syncSummary = "active (bi)"
						ui.stepDone("sync", fmt.Sprintf("active (bi), logs: %s", logPath))
					} else {
						syncSummary = "already active (bi)"
						ui.stepDone("sync", fmt.Sprintf("already active (bi), logs: %s", logPath))
					}
				}
			}
			postCreateCmd := resolvePostCreateCommand(cfg, cfgPath)
			if postCreateCmd != "" {
				ran, err := runPostCreateIfNeeded(k, ns, pod, postCreateCmd, cmd.OutOrStdout(), ui.warnWriter())
				if err != nil {
					return err
				}
				if ran {
					ui.stepDone("postCreate", "completed")
				} else {
					ui.stepDone("postCreate", "already done")
				}
			}

			ui.printWarnings()
			ui.printReadyCard(sn, ns, pod, sshSummary, syncSummary, cfg.Spec.Ports)
			return nil
		},
	}

	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Wait timeout for pod readiness")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without applying resources")
	cmd.Flags().BoolVar(&tmux, "tmux", false, "Enable tmux persistent shell sessions in the sidecar")
	cmd.Flags().StringVar(&sshMode, "ssh-mode", "sidecar", "SSH server mode: sidecar (default) or embedded")
	return cmd
}

func runPostCreateIfNeeded(k *kube.Client, namespace, pod, command string, out io.Writer, errOut io.Writer) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	annotation, err := k.GetPodAnnotation(ctx, namespace, pod, "okdev.io/post-create-done")
	cancel()
	if err != nil {
		fmt.Fprintf(errOut, "warning: failed to read postCreate annotation: %v\n", err)
	}
	if annotation == "true" {
		return false, nil
	}
	fmt.Fprintf(out, "Running postCreate: %s\n", command)
	runCtx, runCancel := context.WithTimeout(context.Background(), 30*time.Minute)
	_, runErr := k.ExecShInContainer(runCtx, namespace, pod, "dev", command)
	runCancel()
	if runErr != nil {
		return true, fmt.Errorf("postCreate failed: %w", runErr)
	}
	annCtx, annCancel := context.WithTimeout(context.Background(), 10*time.Second)
	annErr := k.AnnotatePod(annCtx, namespace, pod, "okdev.io/post-create-done", "true")
	annCancel()
	if annErr != nil {
		fmt.Fprintf(errOut, "warning: failed to annotate pod after postCreate: %v\n", annErr)
	}
	return true, nil
}

func warnIfConfigNewerThanSession(opts *Options, k *kube.Client, namespace, sessionName, podName string, errOut io.Writer) error {
	cfgPath, err := config.ResolvePath(opts.ConfigPath)
	if err != nil {
		return err
	}
	cfgInfo, err := os.Stat(cfgPath)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	podSummary, err := k.GetPodSummary(ctx, namespace, podName)
	if err != nil {
		return err
	}
	if cfgInfo.ModTime().After(podSummary.CreatedAt) {
		fmt.Fprintf(errOut, "warning: config file is newer than running session pod (%s > %s). Run `okdev down --session %s` then `okdev up --session %s` to apply all changes.\n", cfgInfo.ModTime().UTC().Format(time.RFC3339), podSummary.CreatedAt.UTC().Format(time.RFC3339), sessionName, sessionName)
	}
	return nil
}

type upUI struct {
	out      io.Writer
	errOut   io.Writer
	warnings []string
}

func newUpUI(out, errOut io.Writer) *upUI {
	return &upUI{out: out, errOut: errOut}
}

func (u *upUI) section(name string) {
	fmt.Fprintf(u.out, "\n== %s ==\n", name)
}

func (u *upUI) stepRun(step, detail string) {
	if detail == "" {
		fmt.Fprintf(u.out, "… %s\n", step)
		return
	}
	fmt.Fprintf(u.out, "… %s: %s\n", step, detail)
}

func (u *upUI) stepDone(step, detail string) {
	if detail == "" {
		fmt.Fprintf(u.out, "✔ %s\n", step)
		return
	}
	fmt.Fprintf(u.out, "✔ %s: %s\n", step, detail)
}

func (u *upUI) warnf(format string, args ...any) {
	msg := strings.TrimSpace(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}
	u.warnings = append(u.warnings, msg)
}

func (u *upUI) printWarnings() {
	if len(u.warnings) == 0 {
		return
	}
	fmt.Fprintln(u.errOut, "\nWarnings:")
	for _, w := range u.warnings {
		fmt.Fprintf(u.errOut, "- %s\n", w)
	}
}

func (u *upUI) warnWriter() io.Writer {
	return upWarnSink{ui: u}
}

func (u *upUI) printReadyCard(sessionName, namespace, pod, sshSummary, syncSummary string, ports []config.PortMapping) {
	fmt.Fprintln(u.out, "\n== Ready ==")
	fmt.Fprintf(u.out, "session:   %s\n", sessionName)
	fmt.Fprintf(u.out, "namespace: %s\n", namespace)
	fmt.Fprintf(u.out, "pod:       %s\n", pod)
	fmt.Fprintf(u.out, "ssh:       %s\n", sshSummary)
	fmt.Fprintf(u.out, "sync:      %s\n", syncSummary)
	if len(ports) > 0 {
		fmt.Fprintln(u.out, "forwards:")
		for _, p := range ports {
			if p.Local <= 0 || p.Remote <= 0 {
				continue
			}
			name := strings.TrimSpace(p.Name)
			if name == "" {
				name = "port"
			}
			fmt.Fprintf(u.out, "- %s: localhost:%d -> remote:%d\n", name, p.Local, p.Remote)
		}
	}
	fmt.Fprintln(u.out, "next:")
	fmt.Fprintf(u.out, "- ssh %s\n", sshHostAlias(sessionName))
	fmt.Fprintln(u.out, "- okdev status")
	fmt.Fprintln(u.out, "- okdev down")
}

type upWarnSink struct {
	ui *upUI
}

func (s upWarnSink) Write(p []byte) (int, error) {
	if s.ui == nil {
		return len(p), nil
	}
	for _, line := range strings.Split(string(p), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		line = strings.TrimPrefix(line, "warning:")
		line = strings.TrimSpace(line)
		if line != "" {
			s.ui.warnf("%s", line)
		}
	}
	return len(p), nil
}
