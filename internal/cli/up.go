package cli

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

const upDefaultSyncMode = "bi"

func newUpCmd(opts *Options) *cobra.Command {
	var waitTimeout time.Duration
	var dryRun bool
	var tmux bool
	var noTmux bool
	var sshMode string
	var createMissingPVC bool
	var missingPVCSize string
	var missingPVCStorageClass string

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
			annotations["okdev.io/ssh-mode"] = normalizeSSHMode(sshMode)
			volumes := cfg.EffectiveVolumes()
			syncPairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.WorkspaceMountPath())
			if err != nil {
				return err
			}
			pod := podName(sn)
			if dryRun {
				ui.section("Dry Run")
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", sn, ns)
				fmt.Fprintf(cmd.OutOrStdout(), "- using %d configured volume(s)\n", len(volumes))
				if createMissingPVC {
					fmt.Fprintf(cmd.OutOrStdout(), "- would create missing PVC references (size=%s storageClass=%q)\n", missingPVCSize, missingPVCStorageClass)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "- would apply pod/%s\n", pod)
				fmt.Fprintf(cmd.OutOrStdout(), "- would wait for pod readiness (timeout=%s)\n", waitTimeout)
				fmt.Fprintln(cmd.OutOrStdout(), "- would setup SSH config + managed SSH/port-forwards")
				for _, pair := range syncPairs {
					fmt.Fprintf(cmd.OutOrStdout(), "- would sync %s -> %s\n", displayLocalSyncPath(pair.Local), pair.Remote)
				}
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
			if createMissingPVC {
				createdPVCs, err := reconcileMissingPVCs(ctx, k, ns, volumes, missingPVCSize, missingPVCStorageClass, labels, annotations)
				if err != nil {
					return err
				}
				if len(createdPVCs) == 0 {
					ui.stepDone("pvc", "all referenced claims already exist")
				} else {
					ui.stepDone("pvc", "created: "+strings.Join(createdPVCs, ", "))
				}
			} else {
				ui.stepDone("pvc", "not managed (use pre-created PVCs in spec.volumes)")
			}

			if cmd.Flags().Changed("tmux") && cmd.Flags().Changed("no-tmux") {
				return fmt.Errorf("--tmux and --no-tmux cannot be used together")
			}
			enableTmux := cfg.Spec.SSH.PersistentSessionEnabled()
			if cmd.Flags().Changed("tmux") {
				enableTmux = tmux
			}
			if noTmux {
				enableTmux = false
			}
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
			if err := k.AnnotatePod(ctx, ns, pod, "okdev.io/last-attach", time.Now().UTC().Format(time.RFC3339)); err != nil {
				slog.Debug("failed to set last-attach annotation", "error", err)
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

			ui.section("Setup")
			sshSummary := "disabled"
			effectiveSSHPort := sshRemotePortForMode(cfg, sshMode)
			if effectiveSSHPort > 0 {
				keyPath, keyErr := defaultSSHKeyPath(cfg)
				if keyErr != nil {
					return fmt.Errorf("resolve SSH key path: %w", keyErr)
				} else {
					if err := ensureSSHKeyOnPod(opts, cfg, ns, pod, keyPath, sshMode); err != nil {
						return fmt.Errorf("setup SSH key in pod: %w", err)
					}
					if err := waitForSSHDReady(opts, cfg, ns, pod, sshMode, waitTimeout); err != nil {
						return fmt.Errorf("wait for SSH service: %w", err)
					}
					alias := sshHostAlias(sn)
					if _, cfgErr := ensureSSHConfigEntry(alias, sn, ns, cfg.Spec.SSH.User, effectiveSSHPort, keyPath, cfgPath, cfg.Spec.Ports); cfgErr != nil {
						return fmt.Errorf("update ~/.ssh/config: %w", cfgErr)
					} else {
						// Force-refresh managed master so forward rules are always current after `okdev up`.
						_ = stopManagedSSHForward(alias)
						if err := startManagedSSHForwardWithForwards(alias, cfg.Spec.Ports, cfg.Spec.SSH); err != nil {
							return fmt.Errorf("start managed SSH/port-forwards: %w", err)
						} else {
							sshSummary = "ssh " + alias
							ui.stepDone("ssh", sshSummary)
						}
					}
				}
			}

			syncSummary := "disabled"
			syncModeSymbol := ""
			if cfg.Spec.Sync.Engine == "" || cfg.Spec.Sync.Engine == "syncthing" {
				logPath, started, err := startDetachedSyncthingSync(opts, upDefaultSyncMode, sn, ns, cfgPath)
				if err != nil {
					return fmt.Errorf("start syncthing background sync: %w", err)
				} else {
					syncModeSymbol = modeSymbol(upDefaultSyncMode)
					syncPathSummary := syncPairsSummary(syncPairs, syncModeSymbol)
					if started {
						syncSummary = "active (" + syncModeSymbol + ")"
						ui.stepDone("sync", fmt.Sprintf("active (%s), %s, logs: %s", syncModeSymbol, syncPathSummary, logPath))
					} else {
						syncSummary = "already active (" + syncModeSymbol + ")"
						ui.stepDone("sync", fmt.Sprintf("already active (%s), %s, logs: %s", syncModeSymbol, syncPathSummary, logPath))
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
			if enableTmux && sshMode == "embedded" {
				spinner := newTransientStatus(cmd.OutOrStdout(), "Checking tmux in dev container")
				if !spinner.enabled {
					ui.stepRun("tmux", "checking dev container")
				}
				status, installer, err := detectEmbeddedTmux(cmd.Context(), k, ns, pod)
				if err != nil {
					spinner.stop()
					ui.warnf("failed to check tmux in dev container: %v", err)
				} else if status == "install" {
					if spinner.enabled {
						spinner.update(fmt.Sprintf("Installing tmux via %s", installer))
					} else {
						ui.stepRun("tmux", fmt.Sprintf("installing via %s", installer))
					}
					installed, detail, err := installEmbeddedTmux(cmd.Context(), k, ns, pod, installer, func(phase string) {
						if spinner.enabled {
							spinner.update(strings.TrimSpace(phase))
							return
						}
						if strings.TrimSpace(phase) != "" {
							ui.stepRun("tmux", phase)
						}
					})
					spinner.stop()
					if err != nil {
						ui.warnf("failed to install tmux in dev container: %v", err)
					} else if installed {
						ui.stepDone("tmux", detail)
					}
				} else {
					spinner.stop()
					_, detail, err := interpretTmuxStatus(status, installer, "")
					if err != nil {
						ui.warnf("failed to prepare tmux in dev container: %v", err)
					} else {
						ui.stepDone("tmux", detail)
					}
				}
			}
			if err := session.SaveActiveSession(sn); err != nil {
				return err
			}
			ui.stepDone("active session", sn)
			if err := session.ClearShutdownRequest(sn); err != nil {
				ui.warnf("failed to clear prior shutdown request: %v", err)
			}

			ui.printWarnings()
			ui.printReadyCard(sn, ns, pod, sshSummary, syncSummary, cfg.Spec.Ports, syncPairs, syncModeSymbol)
			return nil
		},
	}

	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", 3*time.Minute, "Wait timeout for pod readiness")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without applying resources")
	cmd.Flags().BoolVar(&tmux, "tmux", false, "Enable tmux persistent shell sessions in the sidecar")
	cmd.Flags().BoolVar(&noTmux, "no-tmux", false, "Disable tmux persistent shell sessions for this pod")
	cmd.Flags().StringVar(&sshMode, "ssh-mode", "sidecar", "SSH server mode: sidecar (default) or embedded")
	cmd.Flags().BoolVar(&createMissingPVC, "create-missing-pvc", false, "Create missing PVCs referenced by spec.volumes")
	cmd.Flags().StringVar(&missingPVCSize, "missing-pvc-size", config.DefaultWorkspacePVCSize, "Size to use when creating a missing PVC")
	cmd.Flags().StringVar(&missingPVCStorageClass, "missing-pvc-storage-class", "", "StorageClass to use when creating a missing PVC")
	return cmd
}

func reconcileMissingPVCs(ctx context.Context, k interface {
	PersistentVolumeClaimExists(context.Context, string, string) (bool, error)
	Apply(context.Context, string, []byte) error
}, namespace string, volumes []corev1.Volume, size, storageClass string, labels, annotations map[string]string) ([]string, error) {
	seen := map[string]struct{}{}
	var created []string
	for _, v := range volumes {
		claim := ""
		if v.PersistentVolumeClaim != nil {
			claim = strings.TrimSpace(v.PersistentVolumeClaim.ClaimName)
		}
		if claim == "" {
			continue
		}
		if _, ok := seen[claim]; ok {
			continue
		}
		seen[claim] = struct{}{}
		exists, err := k.PersistentVolumeClaimExists(ctx, namespace, claim)
		if err != nil {
			return nil, fmt.Errorf("check pvc/%s: %w", claim, err)
		}
		if exists {
			continue
		}
		manifest, err := kube.BuildPVCManifest(namespace, claim, size, storageClass, labels, annotations)
		if err != nil {
			return nil, fmt.Errorf("build pvc/%s manifest: %w", claim, err)
		}
		if err := k.Apply(ctx, namespace, manifest); err != nil {
			return nil, fmt.Errorf("create pvc/%s: %w", claim, err)
		}
		created = append(created, claim)
	}
	return created, nil
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

const embeddedTmuxDetectScript = `set -eu
if command -v tmux >/dev/null 2>&1; then
  echo present:none
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo no-root:none
  exit 0
fi
mkdir -p /var/okdev >/dev/null 2>&1 || true
if [ -f /var/okdev/.tmux-install-attempted ]; then
  echo unavailable:none
  exit 0
fi
if command -v apk >/dev/null 2>&1; then
  echo install:apk
elif command -v apt-get >/dev/null 2>&1; then
  echo install:apt-get
elif command -v dnf >/dev/null 2>&1; then
  echo install:dnf
elif command -v microdnf >/dev/null 2>&1; then
  echo install:microdnf
elif command -v yum >/dev/null 2>&1; then
  echo install:yum
else
  echo unavailable:none
fi
`

const embeddedTmuxInstallScript = `set -eu
installer="${OKDEV_TMUX_INSTALLER:-none}"
logfile="/tmp/okdev-tmux-install.log"
mkdir -p /var/okdev >/dev/null 2>&1 || true
touch /var/okdev/.tmux-install-attempted >/dev/null 2>&1 || true
: > "$logfile" 2>/dev/null || true
if command -v tmux >/dev/null 2>&1; then
  echo "__OKDEV_TMUX_STATUS__=installed:${installer}"
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo "__OKDEV_TMUX_STATUS__=no-root:none"
  exit 0
fi
if [ "$installer" = "apk" ]; then
  apk add --no-cache tmux >>"$logfile" 2>&1 || true
elif [ "$installer" = "apt-get" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=10 update >>"$logfile" 2>&1 && apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends tmux >>"$logfile" 2>&1 || true
elif [ "$installer" = "dnf" ]; then
  dnf install -y tmux >>"$logfile" 2>&1 || true
elif [ "$installer" = "microdnf" ]; then
  microdnf install -y tmux >>"$logfile" 2>&1 || true
elif [ "$installer" = "yum" ]; then
  yum install -y tmux >>"$logfile" 2>&1 || true
fi
if command -v tmux >/dev/null 2>&1; then
  echo "__OKDEV_TMUX_STATUS__=installed:${installer}"
  exit 0
fi
echo "__OKDEV_TMUX_STATUS__=unavailable:${installer}"
`

type devShellExecutor interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	StreamShInContainer(context.Context, string, string, string, string, io.Writer, io.Writer) error
}

const embeddedTmuxLogPath = "/tmp/okdev-tmux-install.log"

func detectEmbeddedTmux(ctx context.Context, k devShellExecutor, namespace, pod string) (status, installer string, err error) {
	detectCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	out, err := k.ExecShInContainer(detectCtx, namespace, pod, "dev", embeddedTmuxDetectScript)
	if err != nil {
		return "", "", err
	}
	status, installer, _ = strings.Cut(strings.TrimSpace(string(out)), ":")
	installer = strings.TrimSpace(installer)
	return status, installer, nil
}

func installEmbeddedTmux(ctx context.Context, k devShellExecutor, namespace, pod, installer string, progress func(string)) (bool, string, error) {
	installCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if progress != nil && installer != "" && installer != "none" {
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		started := time.Now()
		done := make(chan struct{})
		defer close(done)
		go func() {
			for {
				select {
				case <-done:
					return
				case <-ticker.C:
					progress(fmt.Sprintf("Installing tmux via %s (%s elapsed)", installer, time.Since(started).Round(time.Second)))
				}
			}
		}()
	}
	script := "export OKDEV_TMUX_INSTALLER=" + shellQuote(installer) + "; " + embeddedTmuxInstallScript
	var raw bytes.Buffer
	err := k.StreamShInContainer(installCtx, namespace, pod, "dev", script, &raw, &raw)
	if err != nil {
		if errors.Is(installCtx.Err(), context.DeadlineExceeded) {
			return false, "", fmt.Errorf("timed out while installing tmux via %s; inspect %s in the dev container", installer, embeddedTmuxLogPath)
		}
		return false, "", err
	}
	s, inst, rawLine := parseTmuxStatus(raw.String())
	if s == "" {
		return false, "", fmt.Errorf("unexpected tmux prepare result %q", strings.TrimSpace(raw.String()))
	}
	inst = strings.TrimSpace(inst)
	return interpretTmuxStatus(s, inst, rawLine)
}

func interpretTmuxStatus(status, installer, raw string) (bool, string, error) {
	hasInstaller := installer != "" && installer != "none"
	switch status {
	case "present":
		return false, "ready in dev container", nil
	case "installed":
		if hasInstaller {
			return true, fmt.Sprintf("installed in dev container via %s", installer), nil
		}
		return true, "installed in dev container", nil
	case "no-root":
		return false, "", fmt.Errorf("dev container is not running as root")
	case "unavailable":
		if hasInstaller {
			return false, "", fmt.Errorf("tmux unavailable after best-effort install attempt via %s; inspect %s in the dev container", installer, embeddedTmuxLogPath)
		}
		return false, "", fmt.Errorf("tmux unavailable (no supported package manager found)")
	default:
		return false, "", fmt.Errorf("unexpected tmux prepare result %q", strings.TrimSpace(raw))
	}
}

func parseTmuxStatus(raw string) (status, installer, line string) {
	scanner := bufio.NewScanner(strings.NewReader(raw))
	for scanner.Scan() {
		text := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(text, "__OKDEV_TMUX_STATUS__=") {
			continue
		}
		line = text
		payload := strings.TrimPrefix(text, "__OKDEV_TMUX_STATUS__=")
		status, installer, _ = strings.Cut(payload, ":")
	}
	return status, installer, line
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

func (u *upUI) printReadyCard(sessionName, namespace, pod, sshSummary, syncSummary string, ports []config.PortMapping, syncPairs []syncengine.Pair, syncModeSymbol string) {
	fmt.Fprintln(u.out, "\n== Ready ==")
	fmt.Fprintf(u.out, "session:   %s\n", sessionName)
	fmt.Fprintf(u.out, "namespace: %s\n", namespace)
	fmt.Fprintf(u.out, "pod:       %s\n", pod)
	fmt.Fprintf(u.out, "ssh:       %s\n", sshSummary)
	fmt.Fprintf(u.out, "sync:      %s\n", syncSummary)
	if len(syncPairs) > 0 {
		fmt.Fprintln(u.out, "sync paths:")
		for _, pair := range syncPairs {
			fmt.Fprintf(u.out, "- %s %s %s\n", displayLocalSyncPath(pair.Local), syncArrow(syncModeSymbol), pair.Remote)
		}
	}
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

func syncPairsSummary(pairs []syncengine.Pair, syncModeSymbol string) string {
	if len(pairs) == 0 {
		return "no paths"
	}
	if len(pairs) == 1 {
		pair := pairs[0]
		return fmt.Sprintf("%s %s %s", displayLocalSyncPath(pair.Local), syncArrow(syncModeSymbol), pair.Remote)
	}
	return fmt.Sprintf("%d paths", len(pairs))
}

func displayLocalSyncPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return path
	}
	if abs, err := filepath.Abs(path); err == nil {
		return abs
	}
	return path
}

func syncArrow(syncModeSymbol string) string {
	if strings.TrimSpace(syncModeSymbol) == "" {
		return "->"
	}
	return syncModeSymbol
}

func modeSymbol(mode string) string {
	switch strings.TrimSpace(mode) {
	case "up":
		return "->"
	case "down":
		return "<-"
	case "bi":
		return "<->"
	default:
		return "->"
	}
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
