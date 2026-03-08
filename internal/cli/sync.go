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

	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

func newSyncCmd(opts *Options) *cobra.Command {
	var mode string
	var background bool
	var dryRun bool

	cmd := &cobra.Command{
		Use:   "sync",
		Short: "Start syncthing sync for session pod",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, ns, err := loadConfigAndNamespace(opts)
			if err != nil {
				return err
			}
			sn, err := resolveSessionName(opts, cfg)
			if err != nil {
				return err
			}
			k := newKubeClient(opts)
			if err := ensureSessionOwnership(opts, k, ns, sn, true); err != nil {
				return err
			}
			engine := cfg.Spec.Sync.Engine
			if engine == "" {
				engine = "syncthing"
			}
			if engine != "syncthing" {
				return fmt.Errorf("unsupported sync engine %q (only syncthing is supported)", engine)
			}
			pairs, err := syncengine.ParsePairs(cfg.Spec.Sync.Paths, cfg.Spec.Workspace.MountPath)
			if err != nil {
				return err
			}
			if dryRun {
				fmt.Fprintf(cmd.OutOrStdout(), "DRY RUN: sync session=%s namespace=%s engine=%s mode=%s\n", sn, ns, engine, mode)
				fmt.Fprintf(cmd.OutOrStdout(), "- paths: %v\n", pairs)
				if background {
					fmt.Fprintln(cmd.OutOrStdout(), "- detached background mode enabled")
				}
				return nil
			}
			stopMaintenance := startSessionMaintenance(opts, cfg, ns, sn, cmd.OutOrStdout(), false, true)
			defer stopMaintenance()

			if background && os.Getenv("OKDEV_SYNCTHING_BACKGROUND_CHILD") != "1" {
				logPath, started, err := startDetachedSyncthingSync(opts, mode, sn)
				if err != nil {
					return err
				}
				if started {
					fmt.Fprintf(cmd.OutOrStdout(), "Started syncthing sync in background for session %s\n", sn)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync already running in background for session %s\n", sn)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Logs: %s\n", logPath)
				return nil
			}
			return runSyncthingSync(cmd, opts, cfg, ns, sn, mode, pairs)
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "bi", "Sync mode: up|down|bi")
	cmd.Flags().BoolVar(&background, "background", false, "Run syncthing sync as a detached background process")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Preview sync actions without transferring files")
	return cmd
}

func startDetachedSyncthingSync(opts *Options, mode, sessionName string) (string, bool, error) {
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
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", false, err
	}

	args := make([]string, 0, 16)
	if opts.ConfigPath != "" {
		args = append(args, "--config", opts.ConfigPath)
	}
	if opts.Session != "" {
		args = append(args, "--session", opts.Session)
	}
	if opts.Namespace != "" {
		args = append(args, "--namespace", opts.Namespace)
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
	time.Sleep(300 * time.Millisecond)
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
	if statOut, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "stat=").CombinedOutput(); err == nil {
		stat := strings.TrimSpace(string(statOut))
		if stat == "" || strings.Contains(stat, "Z") {
			return false
		}
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = p.Signal(syscall.Signal(0))
	return err == nil
}

func processLooksLikeSyncthingSync(pid int) bool {
	if pid <= 0 {
		return false
	}
	out, err := exec.Command("ps", "-p", strconv.Itoa(pid), "-o", "command=").CombinedOutput()
	if err != nil {
		return false
	}
	cmdline := string(out)
	return strings.Contains(cmdline, "okdev") && strings.Contains(cmdline, "sync")
}
