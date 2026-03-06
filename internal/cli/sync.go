package cli

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

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
				logPath, err := startDetachedSyncthingSync(opts, mode, sn)
				if err != nil {
					return err
				}
				fmt.Fprintf(cmd.OutOrStdout(), "Started syncthing sync in background for session %s\n", sn)
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

func startDetachedSyncthingSync(opts *Options, mode, sessionName string) (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	logDir := filepath.Join(home, ".okdev", "logs")
	if err := os.MkdirAll(logDir, 0o755); err != nil {
		return "", err
	}
	logPath := filepath.Join(logDir, "syncthing-"+sessionName+".log")
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return "", err
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
	if err := cmd.Start(); err != nil {
		_ = logFile.Close()
		return "", err
	}
	_ = cmd.Process.Release()
	_ = logFile.Close()
	return logPath, nil
}
