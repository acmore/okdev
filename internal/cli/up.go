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
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
)

const upDefaultSyncMode = "bi"

type upOptions struct {
	waitTimeout            time.Duration
	dryRun                 bool
	reconcile              bool
	tmux                   bool
	noTmux                 bool
	createMissingPVC       bool
	missingPVCSize         string
	missingPVCStorageClass string
}

type upState struct {
	cmd          *cobra.Command
	opts         *Options
	flags        upOptions
	ui           *upUI
	command      *commandContext
	ctx          context.Context
	cancel       context.CancelFunc
	labels       map[string]string
	annotations  map[string]string
	volumes      []corev1.Volume
	syncPairs    []syncengine.Pair
	enableTmux   bool
	runtime      workload.Runtime
	workloadName string
	target       workload.TargetRef
}

func newUpCmd(opts *Options) *cobra.Command {
	var waitTimeout time.Duration
	var dryRun bool
	var reconcile bool
	var tmux bool
	var noTmux bool
	var createMissingPVC bool
	var missingPVCSize string
	var missingPVCStorageClass string

	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create or resume a dev session",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(cmd, opts, upOptions{
				waitTimeout:            waitTimeout,
				dryRun:                 dryRun,
				reconcile:              reconcile,
				tmux:                   tmux,
				noTmux:                 noTmux,
				createMissingPVC:       createMissingPVC,
				missingPVCSize:         missingPVCSize,
				missingPVCStorageClass: missingPVCStorageClass,
			})
		},
	}

	cmd.Flags().DurationVar(&waitTimeout, "wait-timeout", upDefaultWaitTimeout, "Wait timeout for pod readiness")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview actions without applying resources")
	cmd.Flags().BoolVar(&reconcile, "reconcile", false, "Reapply controller-backed workloads instead of reusing an existing session workload")
	cmd.Flags().BoolVar(&tmux, "tmux", false, "Enable tmux persistent shell sessions in the dev container")
	cmd.Flags().BoolVar(&noTmux, "no-tmux", false, "Disable tmux persistent shell sessions for this pod")
	cmd.Flags().BoolVar(&createMissingPVC, "create-missing-pvc", false, "Create missing PVCs referenced by spec.volumes")
	cmd.Flags().StringVar(&missingPVCSize, "missing-pvc-size", config.DefaultWorkspacePVCSize, "Size to use when creating a missing PVC")
	cmd.Flags().StringVar(&missingPVCStorageClass, "missing-pvc-storage-class", "", "StorageClass to use when creating a missing PVC")
	return cmd
}

func runUp(cmd *cobra.Command, opts *Options, flags upOptions) error {
	return withQuietConfigAnnounce(func() error {
		state, err := upValidate(cmd, opts, flags)
		if err != nil {
			return err
		}
		defer state.cancel()

		if err := upReconcile(state, !flags.dryRun); err != nil {
			return err
		}
		if flags.dryRun {
			return upDryRun(state)
		}
		if warnErr := warnIfConfigNewerThanSession(opts, state.command.kube, state.command.namespace, state.command.sessionName, state.workloadName, state.ui.warnWriter()); warnErr != nil {
			slog.Debug("skip config drift warning", "error", warnErr)
		}
		if err := upWait(state); err != nil {
			return err
		}
		return upSetup(state)
	})
}

func upValidate(cmd *cobra.Command, opts *Options, flags upOptions) (*upState, error) {
	ui := newUpUI(cmd.OutOrStdout(), cmd.ErrOrStderr())
	ui.section("Validate")
	cc, err := resolveCommandContext(opts, resolveSessionName)
	if err != nil {
		return nil, err
	}
	ui.stepDone("config", cc.cfgPath)
	ui.stepDone("session", cc.sessionName)
	ui.stepDone("namespace", cc.namespace)
	if cmd.Flags().Changed("tmux") && cmd.Flags().Changed("no-tmux") {
		return nil, fmt.Errorf("--tmux and --no-tmux cannot be used together")
	}
	enableTmux := cc.cfg.Spec.SSH.PersistentSessionEnabled()
	if cmd.Flags().Changed("tmux") {
		enableTmux = flags.tmux
	}
	if flags.noTmux {
		enableTmux = false
	}
	labels := labelsForSession(opts, cc.cfg, cc.sessionName)
	annotations := annotationsForSession(cc.cfg)
	volumes := cc.cfg.EffectiveVolumes()
	syncPairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.WorkspaceMountPath())
	if err != nil {
		return nil, err
	}
	runtime, err := sessionRuntime(cc.cfg, cc.cfgPath, cc.sessionName, labels, annotations, cc.cfg.Spec.PodTemplate.Spec, volumes, enableTmux, resolvePreStopCommand(cc.cfg, cc.cfgPath))
	if err != nil {
		return nil, err
	}
	ctx, cancel := upCommandContext(flags.waitTimeout)
	return &upState{
		cmd:          cmd,
		opts:         opts,
		flags:        flags,
		ui:           ui,
		command:      cc,
		ctx:          ctx,
		cancel:       cancel,
		labels:       labels,
		annotations:  annotations,
		volumes:      volumes,
		syncPairs:    syncPairs,
		enableTmux:   enableTmux,
		runtime:      runtime,
		workloadName: runtime.WorkloadName(),
	}, nil
}

func upReconcile(state *upState, applyWorkload bool) error {
	state.ui.section("Reconcile")
	if err := ensureSessionOwnership(state.opts, state.command.kube, state.command.namespace, state.command.sessionName, true); err != nil {
		return err
	}
	state.ui.stepDone("ownership", "ok")
	if state.flags.createMissingPVC {
		createdPVCs, err := reconcileMissingPVCs(state.ctx, state.command.kube, state.command.namespace, state.volumes, state.flags.missingPVCSize, state.flags.missingPVCStorageClass, state.labels, state.annotations)
		if err != nil {
			return err
		}
		if len(createdPVCs) == 0 {
			state.ui.stepDone("pvc", "all referenced claims already exist")
		} else {
			state.ui.stepDone("pvc", "created: "+strings.Join(createdPVCs, ", "))
		}
	} else {
		state.ui.stepDone("pvc", "not managed (use pre-created PVCs in spec.volumes)")
	}
	if !applyWorkload {
		return nil
	}
	state.ui.stepRun(state.runtime.Kind(), state.workloadName)
	reusedExisting, err := shouldReuseExistingWorkload(state.ctx, state.command.kube, state.command.namespace, state.runtime, state.flags.reconcile)
	if err != nil {
		return err
	}
	if !reusedExisting {
		if err := state.runtime.Apply(state.ctx, state.command.kube, state.command.namespace); err != nil {
			return err
		}
	}
	if reusedExisting {
		state.ui.stepDone(state.runtime.Kind(), "reused existing")
	} else {
		state.ui.stepDone(state.runtime.Kind(), "applied")
	}
	return nil
}

func upDryRun(state *upState) error {
	state.ui.section("Dry Run")
	fmt.Fprintf(state.cmd.OutOrStdout(), "DRY RUN: session=%s namespace=%s\n", state.command.sessionName, state.command.namespace)
	fmt.Fprintf(state.cmd.OutOrStdout(), "- using %d configured volume(s)\n", len(state.volumes))
	if state.flags.createMissingPVC {
		fmt.Fprintf(state.cmd.OutOrStdout(), "- would create missing PVC references (size=%s storageClass=%q)\n", state.flags.missingPVCSize, state.flags.missingPVCStorageClass)
	}
	if state.runtime.Kind() == workload.TypePod || state.flags.reconcile {
		fmt.Fprintf(state.cmd.OutOrStdout(), "- would reconcile %s/%s\n", state.runtime.Kind(), state.workloadName)
	} else {
		fmt.Fprintf(state.cmd.OutOrStdout(), "- would create or reuse %s/%s\n", state.runtime.Kind(), state.workloadName)
	}
	fmt.Fprintf(state.cmd.OutOrStdout(), "- would wait for workload readiness (timeout=%s)\n", state.flags.waitTimeout)
	fmt.Fprintln(state.cmd.OutOrStdout(), "- would setup SSH config + managed SSH/port-forwards")
	for _, pair := range state.syncPairs {
		fmt.Fprintf(state.cmd.OutOrStdout(), "- would sync %s -> %s\n", displayLocalSyncPath(pair.Local), pair.Remote)
	}
	fmt.Fprintln(state.cmd.OutOrStdout(), "- would start background sync (when sync.engine=syncthing)")
	return nil
}

func upWait(state *upState) error {
	state.ui.section("Wait")
	progressPrinter := func(p kube.PodReadinessProgress) {
		reason := strings.TrimSpace(p.Reason)
		if reason == "" {
			reason = "-"
		}
		state.ui.stepRun("pod readiness", fmt.Sprintf("%s %d/%d (%s)", p.Phase, p.ReadyContainers, p.TotalContainers, reason))
	}
	if err := state.runtime.WaitReady(state.ctx, state.command.kube, state.command.namespace, state.flags.waitTimeout, progressPrinter); err != nil {
		hints := fmt.Sprintf("next steps:\n- run `okdev status --session %s`\n- run `kubectl -n %s describe pod %s`", state.command.sessionName, state.command.namespace, state.workloadName)
		diag, derr := state.command.kube.DescribePod(state.ctx, state.command.namespace, state.workloadName)
		if derr == nil {
			fmt.Fprintf(state.cmd.ErrOrStderr(), "pod diagnostics:\n%s\n\n%s\n", diag, hints)
			return fmt.Errorf("wait for %s/%s readiness failed: %w", state.runtime.Kind(), state.workloadName, err)
		}
		fmt.Fprintln(state.cmd.ErrOrStderr(), hints)
		return fmt.Errorf("wait for %s/%s readiness failed: %w", state.runtime.Kind(), state.workloadName, err)
	}
	state.ui.stepDone("pod readiness", "ready")
	target, err := state.runtime.SelectTarget(state.ctx, state.command.kube, state.command.namespace)
	if err != nil {
		return fmt.Errorf("select target: %w", err)
	}
	if err := persistTargetRef(state.command.sessionName, target); err != nil {
		return fmt.Errorf("save target state: %w", err)
	}
	if err := state.command.kube.AnnotatePod(state.ctx, state.command.namespace, target.PodName, "okdev.io/last-attach", time.Now().UTC().Format(time.RFC3339)); err != nil {
		slog.Debug("failed to set last-attach annotation", "error", err)
	}
	state.target = target
	state.ui.stepDone("target", target.PodName+"/"+target.Container)
	return nil
}

func upSetup(state *upState) error {
	state.ui.section("Setup")
	sshSummary := "disabled"
	keyPath, err := defaultSSHKeyPath(state.command.cfg)
	if err != nil {
		return fmt.Errorf("resolve SSH key path: %w", err)
	}
	target, err := refreshTargetRef(state.ctx, state.opts, state.command.cfg, state.command.namespace, state.command.sessionName, state.command.kube, state.target)
	if err != nil {
		return fmt.Errorf("refresh target before setup: %w", err)
	}
	if err := ensureSSHKeyOnPod(state.opts, state.command.namespace, target.PodName, target.Container, keyPath); err != nil {
		return fmt.Errorf("setup SSH key in pod: %w", err)
	}
	target, err = refreshTargetRef(state.ctx, state.opts, state.command.cfg, state.command.namespace, state.command.sessionName, state.command.kube, target)
	if err != nil {
		return fmt.Errorf("refresh target before sshd wait: %w", err)
	}
	if err := waitForSSHReady(state.opts, state.command.namespace, target.PodName, target.Container, state.flags.waitTimeout); err != nil {
		return fmt.Errorf("wait for SSH service: %w", err)
	}
	alias := sshHostAlias(state.command.sessionName)
	if _, cfgErr := ensureSSHConfigEntry(alias, state.command.sessionName, state.command.namespace, state.command.cfg.Spec.SSH.User, sshPort, keyPath, state.command.cfgPath, state.command.cfg.Spec.Ports); cfgErr != nil {
		return fmt.Errorf("update ~/.ssh/config: %w", cfgErr)
	}
	if err := stopManagedSSHForward(alias); err != nil {
		slog.Debug("failed to stop managed SSH forward", "alias", alias, "error", err)
	}
	if err := startManagedSSHForwardWithForwards(alias, state.command.cfg.Spec.Ports, state.command.cfg.Spec.SSH); err != nil {
		return fmt.Errorf("start managed SSH/port-forwards: %w", err)
	}
	sshSummary = "ssh " + alias
	state.ui.stepDone("ssh", sshSummary)

	syncSummary := "disabled"
	syncModeSymbol := ""
	if state.command.cfg.Spec.Sync.Engine == "" || state.command.cfg.Spec.Sync.Engine == "syncthing" {
		if err := refreshSyncthingSessionProcesses(state.command.sessionName); err != nil {
			return fmt.Errorf("refresh local syncthing session state: %w", err)
		}
		logPath, started, err := startDetachedSyncthingSync(state.opts, upDefaultSyncMode, state.command.sessionName, state.command.namespace, state.command.cfgPath)
		if err != nil {
			return fmt.Errorf("start syncthing background sync: %w", err)
		}
		syncModeSymbol = modeSymbol(upDefaultSyncMode)
		syncPathSummary := syncPairsSummary(state.syncPairs, syncModeSymbol)
		if started {
			syncSummary = "active (" + syncModeSymbol + ")"
			state.ui.stepDone("sync", fmt.Sprintf("active (%s), %s, logs: %s", syncModeSymbol, syncPathSummary, logPath))
		} else {
			syncSummary = "already active (" + syncModeSymbol + ")"
			state.ui.stepDone("sync", fmt.Sprintf("already active (%s), %s, logs: %s", syncModeSymbol, syncPathSummary, logPath))
		}
	}

	postCreateCmd := resolvePostCreateCommand(state.command.cfg, state.command.cfgPath)
	if postCreateCmd != "" {
		target, err = refreshTargetRef(state.ctx, state.opts, state.command.cfg, state.command.namespace, state.command.sessionName, state.command.kube, target)
		if err != nil {
			return fmt.Errorf("refresh target before postCreate: %w", err)
		}
		ran, err := runPostCreateIfNeeded(state.command.kube, state.command.namespace, target.PodName, target.Container, postCreateCmd, state.cmd.OutOrStdout(), state.ui.warnWriter())
		if err != nil {
			return err
		}
		if ran {
			state.ui.stepDone("postCreate", "completed")
		} else {
			state.ui.stepDone("postCreate", "already done")
		}
	}
	if state.enableTmux {
		target, err = refreshTargetRef(state.ctx, state.opts, state.command.cfg, state.command.namespace, state.command.sessionName, state.command.kube, target)
		if err != nil {
			return fmt.Errorf("refresh target before tmux setup: %w", err)
		}
		state.ui.stepRun("tmux", "checking dev container")
		status, installer, err := detectDevTmux(state.cmd.Context(), state.command.kube, state.command.namespace, target.PodName, target.Container)
		if err != nil {
			state.ui.warnf("failed to check tmux in dev container: %v", err)
		} else if status == "install" {
			state.ui.stepRun("tmux", fmt.Sprintf("installing via %s", installer))
			_, detail, err := installDevTmux(state.cmd.Context(), state.command.kube, state.command.namespace, target.PodName, target.Container, installer, func(phase string) {
				if strings.TrimSpace(phase) != "" {
					state.ui.stepRun("tmux", phase)
				}
			})
			if err != nil {
				if recoveredDetail, ok := devTmuxDetailIfReady(state.cmd.Context(), state.command.kube, state.command.namespace, target.PodName, target.Container); ok {
					state.ui.stepDone("tmux", recoveredDetail)
				} else {
					state.ui.warnf("tmux not ready in dev container: %v", err)
				}
			} else if strings.TrimSpace(detail) != "" {
				state.ui.stepDone("tmux", detail)
			}
		} else {
			_, detail, err := interpretTmuxStatus(status, installer, "")
			if err != nil {
				state.ui.warnf("tmux not ready in dev container: %v", err)
			} else if strings.TrimSpace(detail) != "" {
				state.ui.stepDone("tmux", detail)
			}
		}
	}
	if err := session.SaveActiveSession(state.command.sessionName); err != nil {
		return err
	}
	state.ui.stepDone("active session", state.command.sessionName)
	if err := session.ClearShutdownRequest(state.command.sessionName); err != nil {
		state.ui.warnf("failed to clear prior shutdown request: %v", err)
	}

	state.ui.printWarnings()
	state.ui.printReadyCard(state.command.sessionName, state.command.namespace, target.PodName, sshSummary, syncSummary, state.command.cfg.Spec.Ports, state.syncPairs, syncModeSymbol)
	return nil
}

type workloadExistenceChecker interface {
	ResourceExists(context.Context, string, string, string, string) (bool, error)
}

func shouldReuseExistingWorkload(ctx context.Context, k workloadExistenceChecker, namespace string, runtime workload.Runtime, reconcile bool) (bool, error) {
	if runtime.Kind() == workload.TypePod || reconcile {
		return false, nil
	}
	refProvider, ok := runtime.(workload.RefProvider)
	if !ok {
		return false, fmt.Errorf("%s runtime does not expose a workload reference", runtime.Kind())
	}
	apiVersion, kind, name, err := refProvider.WorkloadRef()
	if err != nil {
		return false, fmt.Errorf("resolve workload ref: %w", err)
	}
	exists, err := k.ResourceExists(ctx, namespace, apiVersion, kind, name)
	if err != nil {
		return false, fmt.Errorf("check %s/%s existence: %w", kind, name, err)
	}
	return exists, nil
}

func upCommandContext(waitTimeout time.Duration) (context.Context, context.CancelFunc) {
	timeout := defaultContextTimeout
	needed := (2 * waitTimeout) + upContextBuffer
	if needed > timeout {
		timeout = needed
	}
	return context.WithTimeout(context.Background(), timeout)
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

func runPostCreateIfNeeded(k *kube.Client, namespace, pod, container, command string, out io.Writer, errOut io.Writer) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), annotationTimeout)
	annotation, err := k.GetPodAnnotation(ctx, namespace, pod, "okdev.io/post-create-done")
	cancel()
	if err != nil {
		fmt.Fprintf(errOut, "warning: failed to read postCreate annotation: %v\n", err)
	}
	if annotation == "true" {
		return false, nil
	}
	fmt.Fprintf(out, "Running postCreate: %s\n", command)
	runCtx, runCancel := context.WithTimeout(context.Background(), postCreateTimeout)
	_, runErr := k.ExecShInContainer(runCtx, namespace, pod, container, command)
	runCancel()
	if runErr != nil {
		return true, fmt.Errorf("postCreate failed: %w", runErr)
	}
	annCtx, annCancel := context.WithTimeout(context.Background(), annotationTimeout)
	annErr := k.AnnotatePod(annCtx, namespace, pod, "okdev.io/post-create-done", "true")
	annCancel()
	if annErr != nil {
		fmt.Fprintf(errOut, "warning: failed to annotate pod after postCreate: %v\n", annErr)
	}
	return true, nil
}

const devTmuxDetectScript = `set -eu
if command -v tmux >/dev/null 2>&1; then
  echo present:none
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo no-root:none
  exit 0
fi
installer="none"
if command -v apk >/dev/null 2>&1; then
  installer="apk"
elif command -v apt-get >/dev/null 2>&1; then
  installer="apt-get"
elif command -v apt >/dev/null 2>&1; then
  installer="apt"
elif command -v dnf >/dev/null 2>&1; then
  installer="dnf"
elif command -v microdnf >/dev/null 2>&1; then
  installer="microdnf"
elif command -v yum >/dev/null 2>&1; then
  installer="yum"
fi
mkdir -p /var/okdev >/dev/null 2>&1 || true
if [ -f /var/okdev/.tmux-install-attempted ]; then
  echo unavailable:${installer}
  exit 0
fi
if [ "$installer" != "none" ]; then
  echo install:${installer}
else
  echo unavailable:none
fi
`

const devTmuxInstallScript = `set -eu
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
elif [ "$installer" = "apt" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt update >>"$logfile" 2>&1 && apt install -y --no-install-recommends tmux >>"$logfile" 2>&1 || true
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

const devTmuxLogPath = "/tmp/okdev-tmux-install.log"

func detectDevTmux(ctx context.Context, k devShellExecutor, namespace, pod, container string) (status, installer string, err error) {
	detectCtx, cancel := context.WithTimeout(ctx, execProbeTimeout)
	defer cancel()
	out, err := k.ExecShInContainer(detectCtx, namespace, pod, container, devTmuxDetectScript)
	if err != nil {
		return "", "", err
	}
	status, installer, _ = strings.Cut(strings.TrimSpace(string(out)), ":")
	installer = strings.TrimSpace(installer)
	return status, installer, nil
}

func installDevTmux(ctx context.Context, k devShellExecutor, namespace, pod, container, installer string, progress func(string)) (bool, string, error) {
	installCtx := ctx
	if progress != nil && installer != "" && installer != "none" {
		ticker := time.NewTicker(tmuxInstallProgressInterval)
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
	script := "export OKDEV_TMUX_INSTALLER=" + shellQuote(installer) + "; " + devTmuxInstallScript
	var raw bytes.Buffer
	err := k.StreamShInContainer(installCtx, namespace, pod, container, script, &raw, &raw)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return false, "", fmt.Errorf("tmux install cancelled after timeout via parent context; inspect %s in the dev container", devTmuxLogPath)
		}
		if errors.Is(ctx.Err(), context.Canceled) {
			return false, "", fmt.Errorf("tmux install cancelled; inspect %s in the dev container", devTmuxLogPath)
		}
		return false, "", err
	}
	s, inst, rawLine := parseTmuxStatus(raw.String())
	if s == "" {
		detectStatus, detectInstaller, detectErr := detectDevTmux(ctx, k, namespace, pod, container)
		if detectErr == nil {
			return interpretTmuxStatus(detectStatus, detectInstaller, raw.String())
		}
		trimmed := strings.TrimSpace(raw.String())
		if trimmed == "" {
			return false, "", fmt.Errorf("unexpected tmux prepare result with no status marker; inspect %s in the dev container", devTmuxLogPath)
		}
		return false, "", fmt.Errorf("unexpected tmux prepare result %q; inspect %s in the dev container", trimmed, devTmuxLogPath)
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
			return false, "", fmt.Errorf("tmux unavailable after best-effort install attempt via %s; inspect %s in the dev container", installer, devTmuxLogPath)
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

func devTmuxDetailIfReady(ctx context.Context, k devShellExecutor, namespace, pod, container string) (string, bool) {
	status, installer, err := detectDevTmux(ctx, k, namespace, pod, container)
	if err != nil {
		return "", false
	}
	_, detail, err := interpretTmuxStatus(status, installer, "")
	if err != nil || strings.TrimSpace(detail) == "" {
		return "", false
	}
	return detail, true
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
	ctx, cancel := context.WithTimeout(context.Background(), configDriftTimeout)
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
	out         io.Writer
	errOut      io.Writer
	warnings    []string
	interactive bool
	activeStep  string
	active      *transientStatus
}

func newUpUI(out, errOut io.Writer) *upUI {
	return &upUI{
		out:         out,
		errOut:      errOut,
		interactive: isTerminalWriter(out),
	}
}

func (u *upUI) section(name string) {
	u.stopActive()
	fmt.Fprintf(u.out, "\n== %s ==\n", name)
}

func (u *upUI) stepRun(step, detail string) {
	if u.interactive {
		message := u.stepMessage(step, detail)
		if u.active != nil && u.active.enabled {
			if u.activeStep == step {
				u.active.update(message)
				return
			}
			u.stopActive()
		}
		u.activeStep = step
		u.active = newTransientStatusWithMode(u.out, message, true)
		if u.active.enabled {
			return
		}
		u.active = nil
		u.activeStep = ""
	}
	if detail == "" {
		fmt.Fprintf(u.out, "… %s\n", step)
		return
	}
	fmt.Fprintf(u.out, "… %s: %s\n", step, detail)
}

func (u *upUI) stepDone(step, detail string) {
	if u.activeStep == step {
		u.stopActive()
	}
	if detail == "" {
		fmt.Fprintf(u.out, "✔ %s\n", step)
		return
	}
	fmt.Fprintf(u.out, "✔ %s: %s\n", step, detail)
}

func (u *upUI) stepMessage(step, detail string) string {
	if strings.TrimSpace(detail) == "" {
		return step
	}
	return step + ": " + detail
}

func (u *upUI) stopActive() {
	if u.active != nil {
		u.active.stop()
		u.active = nil
	}
	u.activeStep = ""
}

func (u *upUI) warnf(format string, args ...any) {
	msg := strings.TrimSpace(fmt.Sprintf(format, args...))
	if msg == "" {
		return
	}
	u.warnings = append(u.warnings, msg)
}

func (u *upUI) printWarnings() {
	u.stopActive()
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
	u.stopActive()
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
