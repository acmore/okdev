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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/logx"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

type syncthingSessionTargetState struct {
	PodName   string `json:"podName"`
	CreatedAt string `json:"createdAt"`
}

func newSyncCmd(opts *Options) *cobra.Command {
	var mode string
	var background bool
	var foreground bool
	var dryRun bool
	var reset bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Start syncthing sync for session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, true); err != nil {
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
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: sync session=%s namespace=%s engine=%s mode=%s\n", cc.sessionName, cc.namespace, engine, mode)
				fmt.Fprintf(cmd.OutOrStdout(), "- paths: %v\n", pairs)
				if reset {
					fmt.Fprintln(cmd.OutOrStdout(), "- would reset local sync state for this session before setup")
				}
				if background {
					fmt.Fprintln(cmd.OutOrStdout(), "- detached background mode enabled")
				}
				return nil
			}
			stopMaintenance := startSessionMaintenance(opts, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if reset, err := ensureSyncthingTargetSessionState(cmd.Context(), cc.kube, cc.namespace, cc.sessionName, target.PodName); err != nil {
				return err
			} else {
				reportSyncthingTargetReset(cmd.OutOrStdout(), cc.sessionName, reset)
			}

			if reset {
				if err := resetSyncthingSessionState(cc.sessionName); err != nil {
					return err
				}
				if err := saveSyncthingTargetSessionState(cc.sessionName, syncthingSessionTargetState{}); err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Reset local sync state for session %s\n", cc.sessionName)
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
				logPath, started, err := startDetachedSyncthingSync(opts, mode, cc.sessionName, cc.namespace, cc.cfgPath)
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
			return runSyncthingSync(cmd, opts, cc.cfg, cc.namespace, cc.sessionName, mode, pairs, cc.kube)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	cmd.Flags().BoolVar(&background, "background", true, "Run syncthing sync as a detached background process")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run syncthing sync in the foreground for troubleshooting")
	cmd.Flags().BoolVar(&reset, "reset", false, "Stop existing local sync state for this session and bootstrap again")
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
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, true); err != nil {
				return err
			}
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.EffectiveWorkspaceMountPath(cc.cfgPath))
			if err != nil {
				return fmt.Errorf("parse sync paths: %w", err)
			}
			if len(pairs) != 1 {
				return fmt.Errorf("reset-remote requires exactly one sync path mapping, got %d", len(pairs))
			}

			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
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
			logPath, started, err := startDetachedSyncthingSync(opts, "two-phase", cc.sessionName, cc.namespace, cc.cfgPath)
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

func startDetachedSyncthingSync(opts *Options, mode, sessionName, namespace, cfgPath string) (string, bool, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", false, err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", false, err
	}
	logDir := filepath.Join(home, ".okdev", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", false, err
	}
	logPath := filepath.Join(logDir, "syncthing-"+sessionName+".log")
	pidPath, err := syncthingPIDPath(sessionName)
	if err != nil {
		return "", false, err
	}
	readyPath, err := syncthingReadyPath(sessionName)
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

	cmd := exec.Command(exe, args...)
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
	if err := waitForDetachedSyncthingStart(cmd.Process.Pid, readyPath, syncthingDetachedStartupTimeout(mode)); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close log file after early exit", "path", logPath, "error", closeErr)
		}
		return "", false, fmt.Errorf("%w; check logs: %s", err, logPath)
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

func syncthingPIDPath(sessionName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".okdev", "syncthing", sessionName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.pid"), nil
}

func syncthingReadyPath(sessionName string) (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".okdev", "syncthing", sessionName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "sync.ready"), nil
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
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	dir := filepath.Join(home, ".okdev", "syncthing", sessionName)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	return filepath.Join(dir, "target.json"), nil
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
	if strings.TrimSpace(state.PodName) == "" && strings.TrimSpace(state.CreatedAt) == "" {
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
	current := syncthingSessionTargetState{
		PodName:   strings.TrimSpace(summary.Name),
		CreatedAt: summary.CreatedAt.UTC().Format(time.RFC3339Nano),
	}
	previous, err := loadSyncthingTargetSessionState(sessionName)
	if err != nil {
		return false, err
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
	// Try /proc/<pid>/stat first (Linux, no subprocess needed).
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err == nil {
		return procStatIsZombie(data)
	}
	// /proc unavailable (macOS); fall back to ps.
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").CombinedOutput()
	if err != nil {
		return false
	}
	return psStatIsZombie(string(out))
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
