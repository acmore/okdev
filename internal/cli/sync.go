package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/logx"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

var (
	osExecutable                    = os.Executable
	execCommand                     = exec.Command
	waitForDetachedSyncthingStartFn = waitForDetachedSyncthingStart
)

const zombieCheckCacheTTL = 250 * time.Millisecond

type zombieCacheEntry struct {
	zombie bool
	seenAt time.Time
}

var zombieCheckCache sync.Map

type syncthingSessionTargetState struct {
	PodName    string `json:"podName"`
	CreatedAt  string `json:"createdAt"`
	ConfigHash string `json:"configHash,omitempty"`
}

func newSyncCmd(opts *Options) *cobra.Command {
	var mode string
	var background bool
	var foreground bool
	var dryRun bool
	var reset bool
	var force bool
	var resetLocal bool
	var resetMesh bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Start syncthing sync for session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			engine := cc.cfg.Spec.Sync.Engine
			if engine == "" {
				engine = "syncthing"
			}
			if engine != "syncthing" {
				return fmt.Errorf("unsupported sync engine %q (only syncthing is supported)", engine)
			}
			if foreground && cmd.Flags().Changed("background") {
				return fmt.Errorf("--background and --foreground cannot be used together")
			}
			if foreground {
				background = false
			}
			if force && !reset {
				return fmt.Errorf("--force can only be used with --reset")
			}
			if (resetLocal || resetMesh) && !reset {
				return fmt.Errorf("--local and --mesh can only be used with --reset")
			}
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: sync session=%s namespace=%s engine=%s mode=%s\n", cc.sessionName, cc.namespace, engine, mode)
				fmt.Fprintf(cmd.OutOrStdout(), "- paths: %v\n", pairs)
				if reset {
					scope := "all"
					if resetLocal && !resetMesh {
						scope = "local"
					} else if resetMesh && !resetLocal {
						scope = "mesh"
					}
					if force {
						fmt.Fprintf(cmd.OutOrStdout(), "- would force reset %s sync\n", scope)
					} else {
						fmt.Fprintf(cmd.OutOrStdout(), "- would check %s sync health, reset only broken components\n", scope)
					}
				}
				if background {
					fmt.Fprintln(cmd.OutOrStdout(), "- detached background mode enabled")
				}
				return nil
			}
			stopMaintenance := startSessionMaintenanceWithClient(cc.kube, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if reset, err := ensureSyncthingTargetSessionState(cmd.Context(), cc.kube, cc.namespace, cc.sessionName, target.PodName); err != nil {
				return err
			} else {
				reportSyncthingTargetReset(cmd.OutOrStdout(), cc.sessionName, reset)
			}

			if reset {
				wantLocal := !resetMesh || resetLocal
				wantMesh := !resetLocal || resetMesh

				if wantLocal {
					if err := resetLocalSync(cmd, cc, force); err != nil {
						return err
					}
				}
				if wantMesh {
					if force {
						return forceRepairMesh(cmd, cc, target.PodName)
					}
					return repairMeshIfNeeded(cmd, cc, target.PodName)
				}
				return nil
			}

			if !background && mode == "bi" && os.Getenv("OKDEV_SYNCTHING_BACKGROUND_CHILD") != "1" {
				if pidPath, err := syncthingPIDPath(cc.sessionName); err == nil {
					if pid, ok := readSyncthingPID(pidPath); ok && processAlive(pid) && processLooksLikeSyncthingSync(pid) {
						fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync already active in background for session %s\n", cc.sessionName)
						return nil
					}
				}
			}

			if background && os.Getenv("OKDEV_SYNCTHING_BACKGROUND_CHILD") != "1" {
				logPath, started, err := startDetachedSyncthingSync(cc.opts, mode, cc.sessionName, cc.namespace, cc.cfgPath)
				if err != nil {
					return err
				}
				if started {
					fmt.Fprintf(cmd.OutOrStdout(), "Started syncthing sync in background for session %s\n", cc.sessionName)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync already running in background for session %s\n", cc.sessionName)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Logs: %s\n", logPath)
				return nil
			}
			return runSyncthingSync(cmd, cc.opts, cc.cfg, cc.namespace, cc.sessionName, mode, pairs, cc.kube)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	cmd.Flags().BoolVar(&background, "background", true, "Run syncthing sync as a detached background process")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run syncthing sync in the foreground for troubleshooting")
	cmd.Flags().BoolVar(&reset, "reset", false, "Check sync health and reset broken components")
	cmd.Flags().BoolVarP(&force, "force", "f", false, "Force reset without health check (requires --reset)")
	cmd.Flags().BoolVar(&resetLocal, "local", false, "Scope --reset to local-to-hub sync only")
	cmd.Flags().BoolVar(&resetMesh, "mesh", false, "Scope --reset to mesh receivers only")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview sync actions without transferring files")

	cmd.AddCommand(newSyncResetRemoteCmd(opts))
	return cmd
}

func newSyncResetRemoteCmd(opts *Options) *cobra.Command {
	return &cobra.Command{
		Use:   "reset-remote",
		Short: "Clear the remote workspace and re-sync from local",
		Long: `Stop active sync, clear the remote workspace contents (respecting
preservePaths in the sync config), wipe the local Syncthing database,
and re-establish sync via the two-phase initial sync protocol.

The runtime resource and PVC are kept intact — only the managed
synced workspace subtree is cleared.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return fmt.Errorf("parse sync paths: %w", err)
			}
			if len(pairs) != 1 {
				return fmt.Errorf("reset-remote requires exactly one sync path mapping, got %d", len(pairs))
			}

			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Stopping active sync …")
			if err := resetSyncthingSessionState(cc.sessionName); err != nil {
				return fmt.Errorf("stop sync: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Clearing remote workspace …")
			ctx, cancel := context.WithTimeout(cmd.Context(), defaultContextTimeout)
			defer cancel()
			if err := resetRemoteWorkspace(ctx, cc.kube, cc.namespace, target.PodName, pairs[0].Remote, cc.cfg.Spec.Sync.PreservePaths); err != nil {
				return fmt.Errorf("clear remote workspace: %w", err)
			}

			fmt.Fprintln(cmd.OutOrStdout(), "Re-establishing sync …")
			logPath, started, err := startDetachedSyncthingSync(cc.opts, "two-phase", cc.sessionName, cc.namespace, cc.cfgPath)
			if err != nil {
				return fmt.Errorf("restart sync: %w", err)
			}
			if started {
				fmt.Fprintf(cmd.OutOrStdout(), "Sync re-established, logs: %s\n", logPath)
			} else {
				fmt.Fprintf(cmd.OutOrStdout(), "Sync already running, logs: %s\n", logPath)
			}
			return nil
		},
	}
}

func reportSyncthingTargetReset(w io.Writer, sessionName string, reset bool) {
	if !reset {
		return
	}
	fmt.Fprintf(w, "Reset local sync state for session %s: target pod was recreated\n", sessionName)
}

func resetLocalSync(cmd *cobra.Command, cc *commandContext, force bool) error {
	if !force {
		health, reason := checkSyncHealth(cc.sessionName)
		if health == syncHealthActive {
			fmt.Fprintf(cmd.OutOrStdout(), "Local sync: healthy\n")
			return nil
		}
		fmt.Fprintf(cmd.OutOrStdout(), "Local sync: %s (%s)\n", health, reason)
	}

	if err := resetSyncthingSessionState(cc.sessionName); err != nil {
		return err
	}
	if err := saveSyncthingTargetSessionState(cc.sessionName, syncthingSessionTargetState{}); err != nil {
		return err
	}
	fmt.Fprintf(cmd.OutOrStdout(), "Reset local sync state for session %s\n", cc.sessionName)

	logPath, started, err := startDetachedSyncthingSync(cc.opts, "two-phase", cc.sessionName, cc.namespace, cc.cfgPath)
	if err != nil {
		return err
	}
	if started {
		fmt.Fprintf(cmd.OutOrStdout(), "Sync re-established, logs: %s\n", logPath)
	} else {
		fmt.Fprintf(cmd.OutOrStdout(), "Sync already running, logs: %s\n", logPath)
	}
	return nil
}

func meshResetLabels(sessionName string) map[string]string {
	return map[string]string{
		"okdev.io/managed": "true",
		"okdev.io/session": sessionName,
	}
}

func repairMeshIfNeeded(cmd *cobra.Command, cc *commandContext, hubPod string) error {
	labels := meshResetLabels(cc.sessionName)
	ctx, cancel := context.WithTimeout(cmd.Context(), meshSetupTimeout+30*time.Second)
	defer cancel()

	count, err := meshReceiverCount(ctx, cc.kube, cc.namespace, labels)
	if err != nil || count == 0 {
		return nil
	}

	folderID := "okdev-" + cc.sessionName
	pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
	if err != nil {
		return fmt.Errorf("parse sync paths: %w", err)
	}
	folderPath := meshFolderPath(pairs, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))

	fmt.Fprintf(cmd.OutOrStdout(), "Checking mesh health for %d receiver(s) …\n", count)
	health, err := checkMeshHealth(ctx, cc.opts, cc.kube, cc.namespace, cc.sessionName, labels, hubPod, folderID)
	if err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: mesh health check failed: %v\n", err)
		return nil
	}

	broken := brokenMeshReceiverPods(health)
	if len(broken) == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "Mesh: all receivers healthy")
		return nil
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Mesh: %d broken receiver(s): %s\n", len(broken), strings.Join(broken, ", "))
	return doMeshRepair(cmd, ctx, cc, labels, hubPod, folderID, folderPath)
}

func forceRepairMesh(cmd *cobra.Command, cc *commandContext, hubPod string) error {
	labels := meshResetLabels(cc.sessionName)
	ctx, cancel := context.WithTimeout(cmd.Context(), meshSetupTimeout+30*time.Second)
	defer cancel()

	count, err := meshReceiverCount(ctx, cc.kube, cc.namespace, labels)
	if err != nil || count == 0 {
		fmt.Fprintln(cmd.OutOrStdout(), "No mesh receivers found")
		return nil
	}

	folderID := "okdev-" + cc.sessionName
	pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
	if err != nil {
		return fmt.Errorf("parse sync paths: %w", err)
	}
	folderPath := meshFolderPath(pairs, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))

	fmt.Fprintf(cmd.OutOrStdout(), "Force resetting mesh for %d receiver(s) …\n", count)
	return doMeshRepair(cmd, ctx, cc, labels, hubPod, folderID, folderPath)
}

func doMeshRepair(cmd *cobra.Command, ctx context.Context, cc *commandContext, labels map[string]string, hubPod, folderID, folderPath string) error {
	fmt.Fprintln(cmd.OutOrStdout(), "Repairing mesh sync …")
	result, meshErr := repairMeshReceivers(ctx, cc.opts, cc.kube, cc.namespace, cc.sessionName, labels, hubPod, folderID, folderPath, func(status string) {
		fmt.Fprintf(cmd.OutOrStdout(), "  mesh: %s\n", status)
	})
	if meshErr != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "Warning: mesh repair failed: %v\n", meshErr)
		return nil
	}
	if result != nil {
		fmt.Fprintf(cmd.OutOrStdout(), "Mesh: %s\n", formatMeshSummary(result))
		for _, r := range result.Receivers {
			if r.Err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "Warning: mesh receiver %s: %v\n", r.Pod, r.Err)
			}
		}
	}
	return nil
}

func startDetachedSyncthingSync(opts *Options, mode, sessionName, namespace, cfgPath string) (string, bool, error) {
	exe, err := osExecutable()
	if err != nil {
		return "", false, err
	}
	syncthingHome, err := localSyncthingHome(sessionName)
	if err != nil {
		return "", false, err
	}
	logPath, err := localSyncthingLogPath(syncthingHome)
	if err != nil {
		return "", false, err
	}
	pidPath, err := syncthingPIDPath(sessionName)
	if err != nil {
		return "", false, err
	}
	readyPath, err := syncthingReadyPath(sessionName)
	if err != nil {
		return "", false, err
	}
	bootstrapCompletePath, err := syncthingBootstrapCompletePath(sessionName)
	if err != nil {
		return "", false, err
	}
	if pid, ok := readSyncthingPID(pidPath); ok {
		if processAlive(pid) && processLooksLikeSyncthingSync(pid) {
			return logPath, false, nil
		}
		if rmErr := os.Remove(pidPath); rmErr != nil {
			slog.Debug("failed to remove stale PID file", "path", pidPath, "error", rmErr)
		}
	}
	if err := os.Remove(readyPath); err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	if err := os.Remove(bootstrapCompletePath); err != nil && !os.IsNotExist(err) {
		return "", false, err
	}
	logFile, err := logx.OpenRotatingLog(logPath)
	if err != nil {
		return "", false, err
	}

	args := make([]string, 0, 16)
	if cfgPath != "" {
		args = append(args, "--config", cfgPath)
	}
	if sessionName != "" {
		args = append(args, "--session", sessionName)
	}
	if namespace != "" {
		args = append(args, "--namespace", namespace)
	}
	if opts.Context != "" {
		args = append(args, "--context", opts.Context)
	}
	if opts.Verbose {
		args = append(args, "--verbose")
	}
	args = append(args, "sync", "--mode", mode)

	cmd := execCommand(exe, args...)
	cmd.Env = append(os.Environ(), "OKDEV_SYNCTHING_BACKGROUND_CHILD=1")
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close log file after start failure", "path", logPath, "error", closeErr)
		}
		return "", false, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close log file after PID write failure", "path", logPath, "error", closeErr)
		}
		return "", false, err
	}
	if err := waitForDetachedSyncthingStartFn(cmd.Process.Pid, readyPath, syncthingDetachedStartupTimeout(mode)); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close log file after early exit", "path", logPath, "error", closeErr)
		}
		return "", false, formatDetachedSyncthingStartupError(sessionName, logPath, err)
	}
	if releaseErr := cmd.Process.Release(); releaseErr != nil {
		slog.Debug("failed to release detached process", "pid", cmd.Process.Pid, "error", releaseErr)
	}
	if closeErr := logFile.Close(); closeErr != nil {
		slog.Debug("failed to close log file", "path", logPath, "error", closeErr)
	}
	return logPath, true, nil
}

func syncthingDetachedStartupTimeout(mode string) time.Duration {
	if mode == "two-phase" {
		return initialSyncTimeout
	}
	return syncthingBootstrapTimeout
}

func waitForDetachedSyncthingStart(pid int, readyPath string, maxWait time.Duration) error {
	deadline := time.Now().Add(maxWait)
	for {
		if !processAlive(pid) {
			return fmt.Errorf("syncthing background process exited early")
		}
		if processLooksLikeSyncthingSync(pid) && syncthingReady(readyPath) {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("syncthing background process did not become ready")
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func formatDetachedSyncthingStartupError(sessionName, backgroundLogPath string, startupErr error) error {
	msg := fmt.Sprintf("%v; check logs: %s", startupErr, backgroundLogPath)
	if excerpt := readFileTail(backgroundLogPath, 8192); excerpt != "" {
		msg += fmt.Sprintf("\n--- background sync log tail ---\n%s", excerpt)
	}
	if localLogPath, err := syncthingLocalLogPathForSession(sessionName); err == nil {
		if excerpt := readFileTail(localLogPath, 8192); excerpt != "" {
			msg += fmt.Sprintf("\n--- local syncthing log tail ---\n%s", excerpt)
		}
	}
	return errors.New(msg)
}

func syncthingLocalLogPathForSession(sessionName string) (string, error) {
	home, err := localSyncthingHome(sessionName)
	if err != nil {
		return "", err
	}
	return localSyncthingLogPath(home)
}

func readFileTail(path string, maxBytes int) string {
	if strings.TrimSpace(path) == "" || maxBytes <= 0 {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := info.Size() - int64(maxBytes)
	if offset < 0 {
		offset = 0
	}
	if _, err := f.Seek(offset, io.SeekStart); err != nil {
		return ""
	}
	data, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(data))
}

func syncthingPIDPath(sessionName string) (string, error) {
	dir, err := localSyncthingHome(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.pid"), nil
}

func syncthingReadyPath(sessionName string) (string, error) {
	dir, err := localSyncthingHome(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.ready"), nil
}

func syncthingBootstrapCompletePath(sessionName string) (string, error) {
	dir, err := localSyncthingHome(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.bootstrap-complete"), nil
}

func syncthingReady(path string) bool {
	if strings.TrimSpace(path) == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

func stopDetachedSyncthingSync(sessionName string) error {
	pidPath, err := syncthingPIDPath(sessionName)
	if err != nil {
		return err
	}
	pid, ok := readSyncthingPID(pidPath)
	if !ok {
		if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	if processAlive(pid) && processLooksLikeSyncthingSync(pid) {
		p, err := os.FindProcess(pid)
		if err == nil {
			if sigErr := p.Signal(syscall.SIGTERM); sigErr != nil {
				slog.Debug("failed to send SIGTERM to syncthing process", "pid", pid, "error", sigErr)
			}
		}
		deadline := time.Now().Add(syncthingShutdownWait)
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(syncthingShutdownPollInterval)
		}
		if processAlive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				if sigErr := p.Signal(syscall.SIGKILL); sigErr != nil {
					slog.Debug("failed to send SIGKILL to syncthing process", "pid", pid, "error", sigErr)
				}
			}
		}
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

func syncthingTargetStatePath(sessionName string) (string, error) {
	dir, err := session.SessionDir(sessionName)
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.json"), nil
}

func loadSyncthingTargetSessionState(sessionName string) (syncthingSessionTargetState, error) {
	path, err := syncthingTargetStatePath(sessionName)
	if err != nil {
		return syncthingSessionTargetState{}, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return syncthingSessionTargetState{}, nil
		}
		return syncthingSessionTargetState{}, err
	}
	var state syncthingSessionTargetState
	if err := json.Unmarshal(b, &state); err != nil {
		return syncthingSessionTargetState{}, err
	}
	return state, nil
}

func saveSyncthingTargetSessionState(sessionName string, state syncthingSessionTargetState) error {
	path, err := syncthingTargetStatePath(sessionName)
	if err != nil {
		return err
	}
	if strings.TrimSpace(state.PodName) == "" && strings.TrimSpace(state.CreatedAt) == "" && strings.TrimSpace(state.ConfigHash) == "" {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(b, '\n'), 0o644)
}

func saveSyncthingConfigHash(sessionName, configHash string) error {
	state, err := loadSyncthingTargetSessionState(sessionName)
	if err != nil {
		return err
	}
	state.ConfigHash = strings.TrimSpace(configHash)
	return saveSyncthingTargetSessionState(sessionName, state)
}

func syncthingConfigChanged(sessionName, currentHash string) (bool, error) {
	if strings.TrimSpace(currentHash) == "" {
		return false, nil
	}
	state, err := loadSyncthingTargetSessionState(sessionName)
	if err != nil {
		return false, err
	}
	if strings.TrimSpace(state.ConfigHash) == "" {
		return true, nil
	}
	return state.ConfigHash != currentHash, nil
}

type podSummaryReader interface {
	GetPodSummary(context.Context, string, string) (*kube.PodSummary, error)
}

func ensureSyncthingTargetSessionState(ctx context.Context, k podSummaryReader, namespace, sessionName, podName string) (bool, error) {
	if strings.TrimSpace(sessionName) == "" || strings.TrimSpace(podName) == "" {
		return false, nil
	}
	summary, err := k.GetPodSummary(ctx, namespace, podName)
	if err != nil {
		return false, fmt.Errorf("get target pod summary: %w", err)
	}
	previous, err := loadSyncthingTargetSessionState(sessionName)
	if err != nil {
		return false, err
	}
	current := syncthingSessionTargetState{
		PodName:    strings.TrimSpace(summary.Name),
		CreatedAt:  summary.CreatedAt.UTC().Format(time.RFC3339Nano),
		ConfigHash: previous.ConfigHash,
	}
	resetNeeded := previous.PodName != "" && (previous.PodName != current.PodName || previous.CreatedAt != current.CreatedAt)
	if resetNeeded {
		if err := resetSyncthingSessionState(sessionName); err != nil {
			return false, err
		}
	}
	if err := saveSyncthingTargetSessionState(sessionName, current); err != nil {
		return false, err
	}
	return resetNeeded, nil
}

func readSyncthingPID(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	s := strings.TrimSpace(string(b))
	if s == "" {
		return 0, false
	}
	pid, err := strconv.Atoi(s)
	if err != nil || pid <= 0 {
		return 0, false
	}
	return pid, true
}

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return false
	}
	// Signal(0) succeeds for zombies on Unix. Check process state to
	// reject them so stale PID files don't block new sync starts.
	if isZombie(pid) {
		return false
	}
	return true
}

func isZombie(pid int) bool {
	if cached, ok := cachedZombieCheck(pid); ok {
		return cached
	}
	// Try /proc/<pid>/stat first (Linux, no subprocess needed).
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		zombie := procStatIsZombie(data)
		storeZombieCheck(pid, zombie)
		return zombie
	}
	// /proc unavailable (macOS); fall back to ps.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").CombinedOutput()
	if err != nil {
		storeZombieCheck(pid, false)
		return false
	}
	zombie := psStatIsZombie(string(out))
	storeZombieCheck(pid, zombie)
	return zombie
}

func cachedZombieCheck(pid int) (bool, bool) {
	if pid <= 0 {
		return false, false
	}
	v, ok := zombieCheckCache.Load(pid)
	if !ok {
		return false, false
	}
	entry, ok := v.(zombieCacheEntry)
	if !ok || time.Since(entry.seenAt) > zombieCheckCacheTTL {
		zombieCheckCache.Delete(pid)
		return false, false
	}
	return entry.zombie, true
}

func storeZombieCheck(pid int, zombie bool) {
	if pid <= 0 {
		return
	}
	zombieCheckCache.Store(pid, zombieCacheEntry{zombie: zombie, seenAt: time.Now()})
}

func procStatIsZombie(data []byte) bool {
	// Format: "<pid> (comm) <state> ..."  — state is the third field.
	fields := strings.SplitAfterN(string(data), ") ", 2)
	return len(fields) == 2 && len(fields[1]) > 0 && fields[1][0] == 'Z'
}

func psStatIsZombie(stat string) bool {
	return strings.Contains(strings.TrimSpace(stat), "Z")
}

func processLooksLikeSyncthingSync(pid int) bool {
	if pid <= 0 {
		return false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid))
	if err != nil {
		// /proc not available (e.g. macOS); fall back to ps.
		out, psErr := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").CombinedOutput()
		if psErr != nil {
			return false
		}
		cmdline := string(out)
		return strings.Contains(cmdline, "okdev") && strings.Contains(cmdline, "sync")
	}
	cmdline := strings.ReplaceAll(string(data), "\x00", " ")
	return strings.Contains(cmdline, "okdev") && strings.Contains(cmdline, "sync")
}
