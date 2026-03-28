package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/logx"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

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
			cc, err := resolveCommandContext(opts, resolveManagedSessionName)
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
			pairs, err := syncengine.ParsePairs(cc.cfg.Spec.Sync.Paths, cc.cfg.WorkspaceMountPath())
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

			if reset {
				if err := resetSyncthingSessionState(cc.sessionName); err != nil {
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
			return runSyncthingSync(cmd, opts, cc.cfg, cc.namespace, cc.sessionName, mode, pairs)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	cmd.Flags().BoolVar(&background, "background", true, "Run syncthing sync as a detached background process")
	cmd.Flags().BoolVar(&foreground, "foreground", false, "Run syncthing sync in the foreground for troubleshooting")
	cmd.Flags().BoolVar(&reset, "reset", false, "Stop existing local sync state for this session and bootstrap again")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview sync actions without transferring files")
	return cmd
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
	if pid, ok := readSyncthingPID(pidPath); ok {
		if processAlive(pid) && processLooksLikeSyncthingSync(pid) {
			return logPath, false, nil
		}
		_ = os.Remove(pidPath)
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
		_ = logFile.Close()
		return "", false, err
	}
	if err := os.WriteFile(pidPath, []byte(strconv.Itoa(cmd.Process.Pid)+"\n"), 0o644); err != nil {
		_ = logFile.Close()
		return "", false, err
	}
	// Guard against immediate child exit (common when sync init fails).
	time.Sleep(syncthingDetachedStartupDelay)
	if !processAlive(cmd.Process.Pid) || !processLooksLikeSyncthingSync(cmd.Process.Pid) {
		_ = logFile.Close()
		return "", false, fmt.Errorf("syncthing background process exited early; check logs: %s", logPath)
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	return logPath, true, nil
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
			_ = p.Signal(syscall.SIGTERM)
		}
		deadline := time.Now().Add(syncthingShutdownWait)
		for processAlive(pid) && time.Now().Before(deadline) {
			time.Sleep(syncthingShutdownPollInterval)
		}
		if processAlive(pid) {
			if p, err := os.FindProcess(pid); err == nil {
				_ = p.Signal(syscall.SIGKILL)
			}
		}
	}
	if err := os.Remove(pidPath); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
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
