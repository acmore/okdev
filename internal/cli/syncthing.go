package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/logx"
	"github.com/acmore/okdev/internal/session"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/acmore/okdev/internal/syncthing"
	"github.com/spf13/cobra"
)

const (
	syncthingContainerName = "okdev-sidecar"
	localSyncthingGUIAddr  = "127.0.0.1:0"
)

// defaultSyncExcludes are written to a starter .stignore when one does not
// already exist. They prevent common large or noisy directories from being
// synced by default.
var defaultSyncExcludes = []string{".git/", "node_modules", "vendor", "__pycache__", ".DS_Store"}

const (
	largeSyncWarnThreshold = 100 * 1024 * 1024
	largeSyncWarnLimit     = 10
	largeSyncNeedWarnBytes = 500 * 1024 * 1024
)

var syncthingHTTPClient = &http.Client{Timeout: syncthingHTTPClientTimeout}

func runSyncthingSync(cmd *cobra.Command, opts *Options, cfg *config.DevEnvironment, namespace, sessionName, mode string, pairs []syncengine.Pair, k *kube.Client) error {
	if len(pairs) != 1 {
		return fmt.Errorf("syncthing engine currently supports exactly one sync path mapping")
	}

	ctx, cancel := context.WithTimeout(cmd.Context(), syncthingBootstrapTimeout)
	defer cancel()
	localBinary, err := syncthing.EnsureBinary(ctx, cfg.Spec.Sync.Syncthing.Version, cfg.Spec.Sync.Syncthing.AutoInstallEnabled())
	if err != nil {
		return fmt.Errorf("prepare local syncthing binary: %w", err)
	}

	target, err := resolveTargetRef(cmd.Context(), opts, cfg, namespace, sessionName, k)
	if err != nil {
		return err
	}
	pod := target.PodName
	if _, err := execInSyncthingContainer(ctx, k, namespace, pod, "command -v syncthing >/dev/null 2>&1"); err != nil {
		refreshed, refreshErr := resolveTargetRef(cmd.Context(), opts, cfg, namespace, sessionName, k)
		if refreshErr == nil && strings.TrimSpace(refreshed.PodName) != "" && refreshed.PodName != pod {
			pod = refreshed.PodName
			target = refreshed
			if _, retryErr := execInSyncthingContainer(ctx, k, namespace, pod, "command -v syncthing >/dev/null 2>&1"); retryErr == nil {
				err = nil
			} else {
				err = retryErr
			}
		}
		if err != nil {
			return fmt.Errorf("syncthing sidecar not ready; run `okdev up` with sync.engine=syncthing: %w", err)
		}
	}

	pair := pairs[0]
	absLocal, err := filepath.Abs(pair.Local)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(absLocal, 0o755); err != nil {
		return err
	}
	if err := writeLocalSTIgnore(absLocal); err != nil {
		return err
	}
	if _, err := execInSyncthingContainer(ctx, k, namespace, pod, fmt.Sprintf("mkdir -p %s", syncengine.ShellEscape(pair.Remote))); err != nil {
		return err
	}
	localHome, err := localSyncthingHome(sessionName)
	if err != nil {
		return err
	}
	cancelPF, localRemoteAPIBase, localRemotePeerAddr, err := startSyncthingPortForward(ctx, opts, namespace, pod)
	if err != nil {
		return err
	}
	defer cancelPF()

	remoteKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod)
	if err != nil {
		return err
	}

	localBase, localKey, localPID, err := startAndWaitForLocalSyncthing(ctx, localBinary, localHome)
	if err != nil {
		return err
	}
	remoteBase := localRemoteAPIBase
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("remote syncthing not ready: %w", err)
	}

	localID, err := syncthingDeviceID(ctx, localBase, localKey)
	if err != nil {
		return err
	}
	remoteID, err := syncthingDeviceID(ctx, remoteBase, remoteKey)
	if err != nil {
		return err
	}

	folderID := "okdev-" + sessionName
	syncCfg := syncthingSyncConfig{
		rescanInterval: cfg.Spec.Sync.Syncthing.RescanIntervalSeconds,
		watcherDelay:   cfg.Spec.Sync.Syncthing.WatcherDelaySeconds,
		relays:         cfg.Spec.Sync.Syncthing.RelaysEnabled,
		compression:    cfg.Spec.Sync.Syncthing.Compression,
	}

	if err := markSyncthingReady(sessionName); err != nil {
		return fmt.Errorf("mark syncthing ready: %w", err)
	}

	if mode == "two-phase" {
		// The bootstrap context has a short timeout (syncthingBootstrapTimeout).
		// Two-phase initial sync may take much longer, so use a dedicated context.
		twoPhaseCtx, twoPhaseCancel := context.WithTimeout(context.Background(), initialSyncTimeout)
		defer twoPhaseCancel()
		if err := runTwoPhaseInitialSync(twoPhaseCtx, cmd.OutOrStdout(), localBase, localKey, localID, remoteBase, remoteKey, remoteID, localRemotePeerAddr, folderID, absLocal, pair.Remote, syncCfg); err != nil {
			return fmt.Errorf("two-phase initial sync: %w", err)
		}
		mode = "bi"
	} else {
		folderTypeLocal, folderTypeRemote := folderTypesForMode(mode)
		if err := configureSyncthingPeer(ctx, localBase, localKey, localID, remoteID, localRemotePeerAddr, folderID, absLocal, folderTypeLocal, syncCfg.rescanInterval, syncCfg.watcherDelay, false, syncCfg.relays, syncCfg.compression); err != nil {
			return fmt.Errorf("configure local syncthing: %w", err)
		}
		if err := configureSyncthingPeer(ctx, remoteBase, remoteKey, remoteID, localID, "", folderID, pair.Remote, folderTypeRemote, syncCfg.rescanInterval, syncCfg.watcherDelay, false, syncCfg.relays, syncCfg.compression); err != nil {
			return fmt.Errorf("configure remote syncthing: %w", err)
		}
	}

	if err := saveSyncthingConfigHash(sessionName, syncthingSessionConfigHash(cfg, absLocal, pair.Remote)); err != nil {
		return fmt.Errorf("persist syncthing config state: %w", err)
	}

	fmt.Fprintf(cmd.OutOrStdout(), "Syncthing sync active (%s) for session %s\n", mode, sessionName)
	fmt.Fprintf(cmd.OutOrStdout(), "Local folder: %s\n", absLocal)
	fmt.Fprintf(cmd.OutOrStdout(), "Remote folder: %s\n", pair.Remote)
	fmt.Fprintf(cmd.OutOrStdout(), "Local binary: %s\n", localBinary)
	fmt.Fprintln(cmd.OutOrStdout(), "Press Ctrl+C to stop sync tunnel and local syncthing.")

	progressCtx, stopProgress := context.WithCancel(context.Background())
	defer stopProgress()
	go runSyncthingProgressReporter(progressCtx, cmd.OutOrStdout(), localBase, localKey, remoteBase, remoteKey, folderID, localID, remoteID, absLocal)

	// The detached sync child should survive the parent CLI process exiting.
	signal.Ignore(syscall.SIGHUP)
	defer signal.Reset(syscall.SIGHUP)
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sigCh)

	checker := &liveSyncHealthChecker{
		ctx:       ctx,
		opts:      opts,
		namespace: namespace,
		pod:       pod,
		localBase: localBase,
		localKey:  localKey,
		remoteID:  remoteID,
		cancelPF:  cancelPF,
	}
	runSyncHealthLoop(sigCh, cmd.ErrOrStderr(), checker)

	if err := stopOwnedLocalSyncthing(localHome, localPID); err != nil {
		fmt.Fprintf(cmd.ErrOrStderr(), "warning: failed to stop local syncthing: %v\n", err)
	}
	return nil
}

// syncHealthChecker abstracts the peer connectivity check and port-forward
// restoration so the health loop can be tested without real network calls.
type syncHealthChecker interface {
	peerConnected() (bool, error)
	restorePortForward() error
}

// liveSyncHealthChecker is the production implementation that checks the real
// Syncthing API and restores port-forwards via Kubernetes.
type liveSyncHealthChecker struct {
	ctx       context.Context
	opts      *Options
	namespace string
	pod       string
	localBase string
	localKey  string
	remoteID  string
	cancelPF  context.CancelFunc
}

func (c *liveSyncHealthChecker) peerConnected() (bool, error) {
	ctx, cancel := context.WithTimeout(c.ctx, 5*time.Second)
	defer cancel()
	return syncthingPeerConnected(ctx, c.localBase, c.localKey, c.remoteID)
}

func (c *liveSyncHealthChecker) restorePortForward() error {
	newCancelPF, err := restoreSyncPortForward(c.ctx, c.opts, c.namespace, c.pod, c.localBase, c.localKey, c.remoteID, c.cancelPF)
	if err != nil {
		return err
	}
	c.cancelPF = newCancelPF
	return nil
}

// syncHealthLoopConfig holds tunable parameters for the health loop.
type syncHealthLoopConfig struct {
	interval      time.Duration
	quickRetries  int
	backoffFactor int
	maxInterval   time.Duration
	maxRetries    int
}

var defaultSyncHealthLoopConfig = syncHealthLoopConfig{
	interval:      syncHealthCheckInterval,
	quickRetries:  syncHealthCheckQuickRetries,
	backoffFactor: syncHealthCheckBackoffFactor,
	maxInterval:   syncHealthCheckMaxInterval,
	maxRetries:    syncHealthCheckMaxRetries,
}

func syncHealthLoopConfigFromEnv(cfg syncHealthLoopConfig) syncHealthLoopConfig {
	if d, ok := durationFromEnv("OKDEV_SYNC_HEALTH_CHECK_INTERVAL"); ok {
		cfg.interval = d
	}
	if d, ok := durationFromEnv("OKDEV_SYNC_HEALTH_CHECK_MAX_INTERVAL"); ok {
		cfg.maxInterval = d
	}
	return cfg
}

func durationFromEnv(name string) (time.Duration, bool) {
	value := strings.TrimSpace(os.Getenv(name))
	if value == "" {
		return 0, false
	}
	d, err := time.ParseDuration(value)
	if err != nil || d <= 0 {
		slog.Debug("ignoring invalid duration environment override", "name", name, "value", value)
		return 0, false
	}
	return d, true
}

// runSyncHealthLoop monitors Syncthing peer connectivity and restores the
// port-forward when the peer is disconnected. It returns when a signal is
// received on sigCh.
func runSyncHealthLoop(sigCh <-chan os.Signal, errOut io.Writer, checker syncHealthChecker) {
	runSyncHealthLoopWithConfig(sigCh, errOut, checker, syncHealthLoopConfigFromEnv(defaultSyncHealthLoopConfig))
}

func runSyncHealthLoopWithConfig(sigCh <-chan os.Signal, errOut io.Writer, checker syncHealthChecker, cfg syncHealthLoopConfig) {
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	retries := 0
	currentInterval := cfg.interval

	for {
		select {
		case <-sigCh:
			return
		case <-ticker.C:
			connected, err := checker.peerConnected()

			if err != nil {
				// API unreachable — Syncthing may be restarting; skip this tick.
				slog.Debug("sync health check: API unreachable", "error", err)
				continue
			}
			if connected {
				if retries > 0 {
					slog.Info("sync: peer reconnected after restoration")
				}
				retries = 0
				currentInterval = cfg.interval
				ticker.Reset(currentInterval)
				continue
			}

			// Peer disconnected — attempt restoration.
			retries++
			if retries > cfg.maxRetries {
				slog.Error("sync: failed to restore peer connection", "attempts", cfg.maxRetries)
				fmt.Fprintf(errOut, "sync: giving up after %d restoration attempts — run \"okdev sync --reset\" to re-establish sync\n", cfg.maxRetries)
				// Wait for signal to shut down.
				<-sigCh
				return
			}

			slog.Info("sync: peer disconnected, restoring port-forward", "attempt", retries)
			if err := checker.restorePortForward(); err != nil {
				if isFatalSyncRestoreError(err) {
					slog.Error("sync: port-forward restoration hit fatal error, stopping", "error", err)
					fmt.Fprintf(errOut, "sync: cannot restore — %v\n", err)
					<-sigCh
					return
				}
				slog.Warn("sync: port-forward restoration failed", "attempt", retries, "error", err)
			} else {
				slog.Info("sync: port-forward restored, waiting for peer reconnection")
				fmt.Fprintf(errOut, "sync: reconnected to peer\n")
			}

			// Apply backoff after quick retries are exhausted.
			if retries > cfg.quickRetries {
				currentInterval = time.Duration(float64(currentInterval) * float64(cfg.backoffFactor))
				if currentInterval > cfg.maxInterval {
					currentInterval = cfg.maxInterval
				}
			}
			ticker.Reset(currentInterval)
		}
	}
}

// isFatalSyncRestoreError returns true for errors that indicate the target
// workload is permanently gone and retrying would be pointless.
func isFatalSyncRestoreError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	fatal := []string{
		"not found",
		"forbidden",
		"unauthorized",
	}
	for _, s := range fatal {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}

// restoreSyncPortForward cancels the old port-forward, creates a new one,
// and updates the local Syncthing device address to point at the new endpoint.
func restoreSyncPortForward(ctx context.Context, opts *Options, namespace, pod, localBase, localKey, remoteID string, oldCancelPF context.CancelFunc) (context.CancelFunc, error) {
	oldCancelPF()

	newCancelPF, _, newPeerAddr, err := startSyncthingPortForward(ctx, opts, namespace, pod)
	if err != nil {
		return oldCancelPF, fmt.Errorf("start new port-forward: %w", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), syncthingHTTPClientTimeout)
	defer cancel()
	if err := updateSyncthingDeviceAddress(ctx, localBase, localKey, remoteID, newPeerAddr); err != nil {
		newCancelPF()
		return oldCancelPF, fmt.Errorf("update device address: %w", err)
	}

	return newCancelPF, nil
}

func startAndWaitForLocalSyncthing(ctx context.Context, binary, home string) (string, string, int, error) {
	lock, err := lockLocalSyncthingHome(ctx, home)
	if err != nil {
		return "", "", 0, err
	}
	defer lock.Close()

	localProc, err := startLocalSyncthing(binary, home, localSyncthingGUIAddr)
	if err != nil {
		return "", "", 0, err
	}
	localKey, err := readLocalSyncthingAPIKey(home)
	if err != nil {
		_ = stopOwnedLocalSyncthing(home, localProc.PID)
		return "", "", 0, err
	}
	localAPIBase, err := waitLocalSyncthingAPIEndpoint(ctx, localProc, localKey, syncthingAPIReadyTimeout)
	if err != nil {
		_ = stopOwnedLocalSyncthing(home, localProc.PID)
		return "", "", 0, fmt.Errorf("local syncthing not ready: %w", err)
	}
	if err := writeLocalSyncthingEndpoint(home, localAPIBase); err != nil {
		_ = stopOwnedLocalSyncthing(home, localProc.PID)
		return "", "", 0, fmt.Errorf("write local syncthing endpoint: %w", err)
	}
	return localAPIBase, localKey, localProc.PID, nil
}

type localSyncthingHomeLock struct {
	file *os.File
}

func lockLocalSyncthingHome(ctx context.Context, home string) (*localSyncthingHomeLock, error) {
	if strings.TrimSpace(home) == "" {
		return nil, errors.New("syncthing home is empty")
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return nil, fmt.Errorf("create syncthing home: %w", err)
	}
	f, err := os.OpenFile(filepath.Join(home, "local.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("open local syncthing lock: %w", err)
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return &localSyncthingHomeLock{file: f}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = f.Close()
			return nil, fmt.Errorf("lock local syncthing home: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, ctx.Err()
		case <-ticker.C:
		}
	}
}

func (l *localSyncthingHomeLock) Close() {
	if l == nil || l.file == nil {
		return
	}
	_ = syscall.Flock(int(l.file.Fd()), syscall.LOCK_UN)
	_ = l.file.Close()
}

func markSyncthingReady(sessionName string) error {
	path, err := syncthingReadyPath(sessionName)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(time.Now().UTC().Format(time.RFC3339Nano)+"\n"), 0o644)
}

func writeSTIgnore(localPath string, excludes []string) error {
	content, ok := buildSTIgnoreContent(excludes)
	if !ok {
		return nil
	}
	return os.WriteFile(filepath.Join(localPath, ".stignore"), []byte(content), 0o644)
}

func writeLocalSTIgnore(localPath string) error {
	stignorePath := filepath.Join(localPath, ".stignore")
	if _, err := os.Stat(stignorePath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	return writeSTIgnore(localPath, defaultSyncExcludes)
}

func buildSTIgnoreContent(excludes []string) (string, bool) {
	if len(excludes) == 0 {
		return "", false
	}
	var b strings.Builder
	for _, ex := range excludes {
		ex = strings.TrimSpace(ex)
		if ex == "" {
			continue
		}
		b.WriteString(ex)
		if !strings.HasSuffix(ex, "\n") {
			b.WriteString("\n")
		}
	}
	if b.Len() == 0 {
		return "", false
	}
	return b.String(), true
}

func localSyncthingHome(sessionName string) (string, error) {
	return session.SyncthingDir(sessionName)
}

type localSyncthingProcess struct {
	PID       int
	LogPath   string
	LogOffset int64
}

func startLocalSyncthing(binary, home, localGUIAddr string) (localSyncthingProcess, error) {
	genOut, genErr := exec.Command(binary, "generate", "--home", home).CombinedOutput()
	if genErr != nil {
		legacyOut, legacyErr := exec.Command(binary, "-home", home, "generate").CombinedOutput()
		if legacyErr != nil && !strings.Contains(string(genOut), "already exists") && !strings.Contains(string(legacyOut), "already exists") {
			return localSyncthingProcess{}, fmt.Errorf("generate local syncthing config: %w (%s)", genErr, strings.TrimSpace(string(genOut)))
		}
	}

	if err := stopLocalSyncthingForHome(home); err != nil {
		return localSyncthingProcess{}, err
	}
	if endpointPath, pathErr := localSyncthingEndpointPath(home); pathErr == nil {
		if rmErr := os.Remove(endpointPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return localSyncthingProcess{}, fmt.Errorf("remove stale local syncthing endpoint: %w", rmErr)
		}
	}
	logPath, err := localSyncthingLogPath(home)
	if err != nil {
		return localSyncthingProcess{}, err
	}
	logFile, err := openLocalSyncthingLog(logPath)
	if err != nil {
		return localSyncthingProcess{}, fmt.Errorf("open local syncthing log: %w", err)
	}
	logOffset := int64(0)
	if info, statErr := os.Stat(logPath); statErr == nil {
		logOffset = info.Size()
	}
	cmd := newLocalSyncthingServeCommand(binary, home, localGUIAddr)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.Stdin = nil
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close syncthing log file after start failure", "error", closeErr)
		}
		return localSyncthingProcess{}, fmt.Errorf("start local syncthing: %w", err)
	}
	if err := writeLocalSyncthingPID(home, cmd.Process.Pid); err != nil {
		if killErr := cmd.Process.Kill(); killErr != nil {
			slog.Debug("failed to kill syncthing process after PID write failure", "pid", cmd.Process.Pid, "error", killErr)
		}
		if closeErr := logFile.Close(); closeErr != nil {
			slog.Debug("failed to close syncthing log file after PID write failure", "error", closeErr)
		}
		return localSyncthingProcess{}, fmt.Errorf("write local syncthing pid: %w", err)
	}
	if releaseErr := cmd.Process.Release(); releaseErr != nil {
		slog.Debug("failed to release syncthing process", "error", releaseErr)
	}
	if closeErr := logFile.Close(); closeErr != nil {
		slog.Debug("failed to close syncthing log file", "error", closeErr)
	}
	return localSyncthingProcess{PID: cmd.Process.Pid, LogPath: logPath, LogOffset: logOffset}, nil
}

func newLocalSyncthingServeCommand(binary, home, localGUIAddr string) *exec.Cmd {
	cmd := exec.Command(
		binary,
		"serve",
		"--home", home,
		"--no-browser",
		"--gui-address=http://"+localGUIAddr,
		"--no-restart",
		"--skip-port-probing",
		"--no-upgrade",
	)
	cmd.Env = append(os.Environ(), "STNOUPGRADE=1")
	return cmd
}

func stopLocalSyncthing(binary, home string) error {
	if err := stopLocalSyncthingForHome(home); err != nil {
		return fmt.Errorf("stop local syncthing: %w", err)
	}
	return nil
}

func stopOwnedLocalSyncthing(home string, pid int) error {
	if pid <= 0 {
		return nil
	}
	pidPath, err := localSyncthingPIDPath(home)
	if err != nil {
		return err
	}
	currentPID, ok := readSyncthingPID(pidPath)
	if !ok || currentPID != pid {
		return nil
	}
	if err := stopRecordedLocalSyncthing(pid); err != nil {
		return err
	}
	if rmErr := os.Remove(pidPath); rmErr != nil && !os.IsNotExist(rmErr) {
		return rmErr
	}
	return nil
}

// stopLocalSyncthingForSession kills any local syncthing process associated
// with the given session. This is used by `okdev down` as a safety net to
// clean up orphaned syncthing processes that may survive if the detached
// `okdev sync` child is SIGKILL'd before it can run its own cleanup.
func stopLocalSyncthingForSession(sessionName string) error {
	home, err := localSyncthingHome(sessionName)
	if err != nil {
		return err
	}
	if err := stopLocalSyncthingForHome(home); err != nil {
		return fmt.Errorf("stop local syncthing for session %s: %w", sessionName, err)
	}
	return nil
}

func stopLocalSyncthingForHome(home string) error {
	pidPath, err := localSyncthingPIDPath(home)
	if err == nil {
		if pid, ok := readSyncthingPID(pidPath); ok && pid > 0 {
			if stopErr := stopRecordedLocalSyncthing(pid); stopErr != nil {
				return stopErr
			}
			if rmErr := os.Remove(pidPath); rmErr != nil && !os.IsNotExist(rmErr) {
				return rmErr
			}
		} else if rmErr := os.Remove(pidPath); rmErr != nil && !os.IsNotExist(rmErr) {
			return rmErr
		}
	}
	if err := pkillByPattern(syncthingServeHomePattern(home)); err != nil {
		return err
	}
	time.Sleep(syncthingShutdownPollInterval)
	return nil
}

func localSyncthingLogPath(home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errors.New("syncthing home is empty")
	}
	return filepath.Join(home, "local.log"), nil
}

func localSyncthingPIDPath(home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errors.New("syncthing home is empty")
	}
	return filepath.Join(home, "local.pid"), nil
}

func writeLocalSyncthingPID(home string, pid int) error {
	path, err := localSyncthingPIDPath(home)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strconv.Itoa(pid)+"\n"), 0o644)
}

func stopRecordedLocalSyncthing(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	if processAlive(pid) {
		if sigErr := p.Signal(syscall.SIGTERM); sigErr != nil {
			slog.Debug("failed to send SIGTERM to local syncthing", "pid", pid, "error", sigErr)
		}
	}
	deadline := time.Now().Add(syncthingShutdownWait)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(syncthingShutdownPollInterval)
	}
	if processAlive(pid) {
		if sigErr := p.Signal(syscall.SIGKILL); sigErr != nil {
			slog.Debug("failed to send SIGKILL to local syncthing", "pid", pid, "error", sigErr)
		}
	}
	deadline = time.Now().Add(syncthingShutdownWait)
	for processAlive(pid) && time.Now().Before(deadline) {
		time.Sleep(syncthingShutdownPollInterval)
	}
	return nil
}

func localSyncthingEndpointPath(home string) (string, error) {
	if strings.TrimSpace(home) == "" {
		return "", errors.New("syncthing home is empty")
	}
	return filepath.Join(home, "endpoint"), nil
}

func writeLocalSyncthingEndpoint(home, apiBase string) error {
	path, err := localSyncthingEndpointPath(home)
	if err != nil {
		return err
	}
	return os.WriteFile(path, []byte(strings.TrimSpace(apiBase)+"\n"), 0o644)
}

func openLocalSyncthingLog(path string) (*os.File, error) {
	return logx.OpenRotatingLog(path)
}

func pkillByPattern(pattern string) error {
	cmd := exec.Command("pkill", "-f", pattern)
	out, err := cmd.CombinedOutput()
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	msg := strings.TrimSpace(string(out))
	if errors.As(err, &exitErr) && (exitErr.ExitCode() == 1 || strings.Contains(msg, "sysmond service not found")) {
		return nil
	}
	return fmt.Errorf("%w (%s)", err, msg)
}

func syncthingServeHomePattern(home string) string {
	return "serve --home " + regexp.QuoteMeta(home)
}

func resetSyncthingSessionState(sessionName string) error {
	if err := stopDetachedSyncthingSync(sessionName); err != nil {
		return fmt.Errorf("stop background sync: %w", err)
	}
	if err := stopLocalSyncthingForSession(sessionName); err != nil {
		return fmt.Errorf("stop local syncthing: %w", err)
	}
	home, err := localSyncthingHome(sessionName)
	if err != nil {
		return err
	}
	if err := os.RemoveAll(home); err != nil {
		return fmt.Errorf("remove local syncthing state for session %s: %w", sessionName, err)
	}
	if err := os.MkdirAll(home, 0o755); err != nil {
		return fmt.Errorf("recreate local syncthing state dir for session %s: %w", sessionName, err)
	}
	return nil
}

func refreshSyncthingSessionProcesses(sessionName string) error {
	if err := stopDetachedSyncthingSync(sessionName); err != nil {
		return fmt.Errorf("stop background sync: %w", err)
	}
	if err := stopLocalSyncthingForSession(sessionName); err != nil {
		return fmt.Errorf("stop local syncthing: %w", err)
	}
	return nil
}

func startSyncthingPortForward(ctx context.Context, opts *Options, namespace, pod string) (context.CancelFunc, string, string, error) {
	lastErr := error(nil)
	for i := 0; i < 5; i++ {
		localAPI, err := reserveEphemeralPort()
		if err != nil {
			return nil, "", "", err
		}
		localSync, err := reserveEphemeralPort()
		if err != nil {
			return nil, "", "", err
		}
		cancelPF, err := startManagedPortForward(ctx, opts, namespace, pod, []string{
			fmt.Sprintf("%d:8384", localAPI),
			fmt.Sprintf("%d:22000", localSync),
		})
		if err == nil {
			return cancelPF, fmt.Sprintf("http://127.0.0.1:%d", localAPI), fmt.Sprintf("tcp://127.0.0.1:%d", localSync), nil
		}
		lastErr = err
	}
	return nil, "", "", lastErr
}

type syncthingConfigXML struct {
	XMLName xml.Name `xml:"configuration"`
	GUI     struct {
		APIKey  string `xml:"apikey"`
		Address string `xml:"address"`
	} `xml:"gui"`
}

func readLocalSyncthingAPIKey(home string) (string, error) {
	paths := []string{filepath.Join(home, "config.xml"), filepath.Join(home, "config", "config.xml")}
	for _, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		var cfg syncthingConfigXML
		if err := xml.Unmarshal(b, &cfg); err != nil {
			continue
		}
		if cfg.GUI.APIKey != "" {
			return cfg.GUI.APIKey, nil
		}
	}
	return "", errors.New("unable to read local syncthing API key")
}

func readRemoteSyncthingAPIKey(ctx context.Context, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod string) (string, error) {
	out, err := execInSyncthingContainer(ctx, k, namespace, pod, "cat /var/syncthing/config.xml 2>/dev/null || cat /var/syncthing/config/config.xml")
	if err != nil {
		return "", err
	}
	var cfg syncthingConfigXML
	if err := xml.Unmarshal(out, &cfg); err != nil {
		return "", err
	}
	if cfg.GUI.APIKey == "" {
		return "", errors.New("remote syncthing API key is empty")
	}
	return cfg.GUI.APIKey, nil
}

func execInSyncthingContainer(ctx context.Context, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod, script string) ([]byte, error) {
	return k.ExecShInContainer(ctx, namespace, pod, syncthingContainerName, script)
}

func waitSyncthingAPI(ctx context.Context, base, key string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(syncthingAPIReadyPollInterval)
	defer ticker.Stop()
	for time.Now().Before(deadline) {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}
		_, err := syncthingDeviceID(ctx, base, key)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return errors.New("API did not become ready")
}

func waitLocalSyncthingAPIEndpoint(ctx context.Context, proc localSyncthingProcess, key string, timeout time.Duration) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	ticker := time.NewTicker(syncthingAPIReadyPollInterval)
	defer ticker.Stop()

	var lastErr error
	for {
		logText := readFileFromOffset(proc.LogPath, proc.LogOffset, 8192)
		if base, ok := parseLocalSyncthingAPIBase(logText); ok {
			if _, err := syncthingDeviceID(ctx, base, key); err == nil {
				return base, nil
			} else {
				lastErr = err
			}
		}

		select {
		case <-ctx.Done():
			if lastErr != nil {
				return "", lastErr
			}
			return "", ctx.Err()
		case <-ticker.C:
		}
	}
}

var syncthingGUIListenPattern = regexp.MustCompile(`GUI and API listening on (\[[^\]]+\]:\d+|[^\s/]+:\d+)`)

func parseLocalSyncthingAPIBase(logText string) (string, bool) {
	matches := syncthingGUIListenPattern.FindAllStringSubmatch(logText, -1)
	for i := len(matches) - 1; i >= 0; i-- {
		if base, ok := normalizeLocalSyncthingAPIBase(matches[i][1]); ok {
			return base, true
		}
	}
	return "", false
}

func normalizeLocalSyncthingAPIBase(addr string) (string, bool) {
	addr = strings.TrimSpace(strings.TrimRight(addr, "/"))
	if addr == "" {
		return "", false
	}
	if !strings.Contains(addr, "://") {
		addr = "http://" + addr
	}
	u, err := url.Parse(addr)
	if err != nil || u.Host == "" {
		return "", false
	}
	host := u.Hostname()
	port := u.Port()
	if port == "" || !isLoopbackHost(host) {
		return "", false
	}
	return "http://" + net.JoinHostPort(host, port), true
}

func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func readFileFromOffset(path string, offset int64, maxBytes int64) string {
	if strings.TrimSpace(path) == "" || maxBytes <= 0 {
		return ""
	}
	data, err := os.ReadFile(path)
	if err != nil || len(data) == 0 {
		return ""
	}
	if offset < 0 {
		offset = 0
	}
	if offset > int64(len(data)) {
		offset = int64(len(data))
	}
	data = data[offset:]
	if int64(len(data)) > maxBytes {
		data = data[len(data)-int(maxBytes):]
	}
	return strings.TrimSpace(string(data))
}

func syncthingDeviceID(ctx context.Context, base, key string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/rest/system/status", nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("X-API-Key", key)
	resp, err := syncthingHTTPClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return "", fmt.Errorf("status %s", resp.Status)
	}
	var payload struct {
		MyID string `json:"myID"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return "", err
	}
	if payload.MyID == "" {
		return "", errors.New("empty myID from syncthing")
	}
	return payload.MyID, nil
}

func runSyncthingProgressReporter(ctx context.Context, out io.Writer, localBase, localKey, remoteBase, remoteKey, folderID, localID, remoteID, localPath string) {
	ticker := time.NewTicker(syncthingProgressInterval)
	defer ticker.Stop()
	warnedLargeSync := false
	emit := func() bool {
		upPct, upNeed, err := syncthingCompletion(ctx, localBase, localKey, folderID, remoteID)
		if err != nil {
			slog.Debug("syncthing progress read failed", "side", "local", "error", err)
			return false
		}
		downPct, downNeed, err := syncthingCompletion(ctx, remoteBase, remoteKey, folderID, localID)
		if err != nil {
			slog.Debug("syncthing progress read failed", "side", "remote", "error", err)
			return false
		}
		fmt.Fprintf(out, "Syncthing progress: %s\n", formatInitialSyncProgressDetail(syncthingInitialSyncProgress{
			Mode:            "bi",
			LocalNeedBytes:  upNeed,
			RemoteNeedBytes: downNeed,
		}))
		if !warnedLargeSync && maxInt64(upNeed, downNeed) >= largeSyncNeedWarnBytes {
			warnLargeSyncEntries(out, localPath, maxInt64(upNeed, downNeed))
			warnedLargeSync = true
		}
		if syncthingInitialSyncComplete(upPct, upNeed, downPct, downNeed) {
			return true
		}
		return false
	}
	if emit() {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if emit() {
				return
			}
		}
	}
}

func maxInt64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func syncthingCompletion(ctx context.Context, base, key, folderID, deviceID string) (float64, int64, error) {
	path := fmt.Sprintf("/rest/db/completion?folder=%s&device=%s", url.QueryEscape(folderID), url.QueryEscape(deviceID))
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, path, nil, "")
	if err != nil {
		return 0, 0, err
	}
	var payload struct {
		Completion float64 `json:"completion"`
		NeedBytes  int64   `json:"needBytes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, 0, err
	}
	return payload.Completion, payload.NeedBytes, nil
}

type syncthingInitialSyncProgress struct {
	Mode string
	// LocalNeedBytes is the number of bytes the remote device still needs,
	// as reported by querying the local Syncthing completion endpoint for
	// the remote device ID.  This represents local→remote (upload) traffic.
	LocalNeedBytes int64
	// RemoteNeedBytes is the number of bytes the local device still needs,
	// as reported by querying the remote Syncthing completion endpoint for
	// the local device ID.  This represents remote→local (download) traffic.
	RemoteNeedBytes   int64
	UploadBps         int64
	DownloadBps       int64
	HasUploadBps      bool
	HasDownloadBps    bool
	NeedWarnLargeSync bool
	MaxNeedBytes      int64
}

func syncthingInitialSyncComplete(localPct float64, localNeedBytes int64, remotePct float64, remoteNeedBytes int64) bool {
	return localNeedBytes == 0 && remoteNeedBytes == 0 && localPct >= 99.9 && remotePct >= 99.9
}

func syncthingBootstrapInitialSyncComplete(localPct float64, localNeedBytes int64) bool {
	return localNeedBytes == 0 && localPct >= 99.9
}

// syncthingFolderNeedBytes queries the remote syncthing for the number of
// bytes still needed for the given folder. Returns 0 when sync is complete.
func syncthingFolderNeedBytes(ctx context.Context, base, key, folderID string) (int64, error) {
	path := fmt.Sprintf("/rest/db/status?folder=%s", url.QueryEscape(folderID))
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, path, nil, "")
	if err != nil {
		return 0, err
	}
	var payload struct {
		NeedBytes int64 `json:"needBytes"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return 0, err
	}
	return payload.NeedBytes, nil
}

func syncthingPeerConnected(ctx context.Context, base, key, peerID string) (bool, error) {
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, "/rest/system/connections", nil, "")
	if err != nil {
		return false, err
	}
	var conns struct {
		Connections map[string]struct {
			Connected bool `json:"connected"`
		} `json:"connections"`
	}
	if err := json.Unmarshal(body, &conns); err != nil {
		return false, err
	}
	if strings.TrimSpace(peerID) == "" {
		for _, c := range conns.Connections {
			if c.Connected {
				return true, nil
			}
		}
		return false, nil
	}
	if c, ok := conns.Connections[peerID]; ok {
		return c.Connected, nil
	}
	return false, nil
}

type syncthingConnectionStats struct {
	InBytesTotal  int64 `json:"inBytesTotal"`
	OutBytesTotal int64 `json:"outBytesTotal"`
}

func syncthingPeerConnectionTotals(ctx context.Context, base, key, peerID string) (syncthingConnectionStats, error) {
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, "/rest/system/connections", nil, "")
	if err != nil {
		return syncthingConnectionStats{}, err
	}
	var conns struct {
		Connections map[string]syncthingConnectionStats `json:"connections"`
	}
	if err := json.Unmarshal(body, &conns); err != nil {
		return syncthingConnectionStats{}, err
	}
	stats, ok := conns.Connections[peerID]
	if !ok {
		return syncthingConnectionStats{}, fmt.Errorf("peer %s not found in syncthing connections", peerID)
	}
	return stats, nil
}

type syncthingRateEstimator struct {
	lastAt      time.Time
	lastBytes   int64
	initialized bool
}

func (e *syncthingRateEstimator) Estimate(now time.Time, totalBytes int64) (int64, bool) {
	if !e.initialized {
		e.lastAt = now
		e.lastBytes = totalBytes
		e.initialized = true
		return 0, false
	}
	elapsed := now.Sub(e.lastAt)
	delta := totalBytes - e.lastBytes
	e.lastAt = now
	e.lastBytes = totalBytes
	if elapsed <= 0 || delta < 0 {
		return 0, false
	}
	return int64(float64(delta) / elapsed.Seconds()), true
}

// waitForInitialSync blocks until both local→remote and remote→local pending
// bytes reach zero for the managed folder. It opens a temporary port-forward to
// the target pod's Syncthing sidecar and reads the local Syncthing endpoint
// from session state so `okdev up` only continues once initial sync has fully
// converged in both directions.
//
// onStatus is called during the setup phase (port-forward, API readiness, etc.)
// so the caller can surface intermediate progress before polling begins.
//
// When bootstrapResume is true the function inspects the remote folder config
// (reusing the port-forward it already opened) and auto-upgrades the effective
// mode to "two-phase" if a previous bootstrap did not finish.  This avoids
// opening a second port-forward just for the probe.
func waitForInitialSync(ctx context.Context, opts *Options, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod, sessionName, mode string, bootstrapResume bool, timeout time.Duration, onStatus func(string), onProgress func(syncthingInitialSyncProgress)) error {
	folderID := "okdev-" + sessionName
	localHome, err := localSyncthingStatusHome(sessionName)
	if err != nil {
		return fmt.Errorf("resolve local syncthing home: %w", err)
	}
	localBase, localKey, err := readLocalSyncthingEndpoint(localHome)
	if err != nil {
		return fmt.Errorf("read local syncthing endpoint: %w", err)
	}

	if onStatus != nil {
		onStatus("reading remote syncthing credentials")
	}
	remoteKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod)
	if err != nil {
		return fmt.Errorf("read remote syncthing API key: %w", err)
	}

	if onStatus != nil {
		onStatus("connecting to syncthing sidecar")
	}
	cancelPF, remoteBase, _, err := startSyncthingPortForward(ctx, opts, namespace, pod)
	if err != nil {
		return fmt.Errorf("port-forward to syncthing sidecar: %w", err)
	}
	defer cancelPF()

	if onStatus != nil {
		onStatus("waiting for syncthing API")
	}
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("remote syncthing not ready: %w", err)
	}
	if err := waitSyncthingAPI(ctx, localBase, localKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("local syncthing not ready: %w", err)
	}

	// If the caller indicates a prior config state exists, reuse the
	// already-open port-forward to probe whether the previous bootstrap
	// finished.  This avoids a second port-forward round-trip.
	if bootstrapResume && mode != "two-phase" {
		if onStatus != nil {
			onStatus("checking bootstrap state")
		}
		cfg, cfgErr := syncthingGetConfig(ctx, remoteBase, remoteKey)
		if cfgErr != nil {
			slog.Debug("failed to read remote syncthing config for bootstrap probe", "error", cfgErr)
		} else {
			folderType, found, ftErr := syncthingManagedFolderType(cfg, folderID)
			if ftErr != nil {
				slog.Debug("failed to inspect remote syncthing folder for bootstrap probe", "error", ftErr)
			} else if shouldResumeTwoPhaseBootstrap(folderType, found) {
				mode = "two-phase"
			}
		}
	}

	if onStatus != nil {
		onStatus("reading device identities")
	}
	localID, err := syncthingDeviceID(ctx, localBase, localKey)
	if err != nil {
		return fmt.Errorf("read local syncthing device id: %w", err)
	}
	remoteID, err := syncthingDeviceID(ctx, remoteBase, remoteKey)
	if err != nil {
		return fmt.Errorf("read remote syncthing device id: %w", err)
	}

	deadline := time.Now().Add(timeout)
	ticker := time.NewTicker(syncthingProgressInterval)
	defer ticker.Stop()
	uploadRate := &syncthingRateEstimator{}
	downloadRate := &syncthingRateEstimator{}

	// Fire the first poll immediately so the user sees progress without
	// waiting for the initial tick interval (5 s).

	for {
		localPct, localNeed, localErr := syncthingCompletion(ctx, localBase, localKey, folderID, remoteID)
		remotePct, remoteNeed, remoteErr := syncthingCompletion(ctx, remoteBase, remoteKey, folderID, localID)
		connStats, connErr := syncthingPeerConnectionTotals(ctx, localBase, localKey, remoteID)
		if localErr != nil {
			slog.Debug("syncthing initial sync local poll failed", "error", localErr)
		}
		if remoteErr != nil {
			slog.Debug("syncthing initial sync remote poll failed", "error", remoteErr)
		}
		if connErr != nil {
			slog.Debug("syncthing initial sync connection stats poll failed", "error", connErr)
		}
		if localErr == nil && remoteErr == nil {
			var uploadBps, downloadBps int64
			var hasUploadBps, hasDownloadBps bool
			if connErr == nil {
				now := time.Now()
				uploadBps, hasUploadBps = uploadRate.Estimate(now, connStats.OutBytesTotal)
				downloadBps, hasDownloadBps = downloadRate.Estimate(now, connStats.InBytesTotal)
			}
			maxNeed := maxInt64(localNeed, remoteNeed)
			if onProgress != nil {
				onProgress(syncthingInitialSyncProgress{
					Mode:              mode,
					LocalNeedBytes:    localNeed,
					RemoteNeedBytes:   remoteNeed,
					UploadBps:         uploadBps,
					DownloadBps:       downloadBps,
					HasUploadBps:      hasUploadBps,
					HasDownloadBps:    hasDownloadBps,
					NeedWarnLargeSync: maxNeed >= largeSyncNeedWarnBytes,
					MaxNeedBytes:      maxNeed,
				})
			}
			if mode == "two-phase" {
				if syncthingBootstrapInitialSyncComplete(localPct, localNeed) {
					return nil
				}
			} else if syncthingInitialSyncComplete(localPct, localNeed, remotePct, remoteNeed) {
				return nil
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("initial sync did not complete within %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func formatSyncthingMiB(bytes int64) string {
	if bytes <= 0 {
		return "0.0 MiB"
	}
	return fmt.Sprintf("%.1f MiB", float64(bytes)/(1024*1024))
}

func formatSyncthingSpeed(bps int64) string {
	if bps <= 0 {
		return "0.0 MiB/s"
	}
	return fmt.Sprintf("%.1f MiB/s", float64(bps)/(1024*1024))
}

func formatInitialSyncProgressDetail(progress syncthingInitialSyncProgress) string {
	if progress.Mode == "two-phase" {
		speed := ""
		if progress.HasUploadBps {
			speed = fmt.Sprintf(" at %s", formatSyncthingSpeed(progress.UploadBps))
		}
		return fmt.Sprintf(
			"%s remaining to remote%s",
			formatSyncthingMiB(progress.LocalNeedBytes),
			speed,
		)
	}
	totalNeedBytes := progress.LocalNeedBytes + progress.RemoteNeedBytes
	speed := ""
	if progress.HasUploadBps || progress.HasDownloadBps {
		up := "measuring"
		down := "measuring"
		if progress.HasUploadBps {
			up = formatSyncthingSpeed(progress.UploadBps)
		}
		if progress.HasDownloadBps {
			down = formatSyncthingSpeed(progress.DownloadBps)
		}
		speed = fmt.Sprintf(", up %s, down %s", up, down)
	}
	return fmt.Sprintf(
		"%s remaining (local->remote %s, remote->local %s)%s",
		formatSyncthingMiB(totalNeedBytes),
		formatSyncthingMiB(progress.LocalNeedBytes),
		formatSyncthingMiB(progress.RemoteNeedBytes),
		speed,
	)
}

func formatTwoPhaseNeedProgress(label string, needBytes int64) string {
	return fmt.Sprintf("%s: %s remaining", label, formatSyncthingMiB(needBytes))
}

// syncthingSyncConfig bundles the user-configurable knobs passed through to
// configureSyncthingPeer so call sites stay compact.
type syncthingSyncConfig struct {
	rescanInterval int
	watcherDelay   int
	relays         bool
	compression    bool
}

// runTwoPhaseInitialSync implements the Okteto-style two-phase sync protocol:
//
//  1. Configure local as sendonly with ignoreDelete=true, remote as receiveonly.
//     Call POST /rest/db/override on every progress tick to force local state
//     onto the remote.
//     1b. Once needBytes==0, flip ignoreDelete to false and override once more so
//     that files deleted locally (branch switch, git pull) are propagated as
//     deletions before entering bidirectional mode.
//  2. Reconfigure both sides to sendreceive and restart only if Syncthing
//     reports that the applied config requires it.
func runTwoPhaseInitialSync(ctx context.Context, out io.Writer, localBase, localKey, localID, remoteBase, remoteKey, remoteID, peerAddr, folderID, localPath, remotePath string, sc syncthingSyncConfig) error {
	// Phase 1: sendonly + ignoreDelete on local, receiveonly on remote.
	fmt.Fprintln(out, "Phase 1: local-authoritative initial sync …")
	if err := configureSyncthingPeer(ctx, localBase, localKey, localID, remoteID, peerAddr, folderID, localPath, "sendonly", sc.rescanInterval, sc.watcherDelay, true, sc.relays, sc.compression); err != nil {
		return fmt.Errorf("configure local (phase 1): %w", err)
	}
	if err := configureSyncthingPeer(ctx, remoteBase, remoteKey, remoteID, localID, "", folderID, remotePath, "receiveonly", sc.rescanInterval, sc.watcherDelay, false, sc.relays, sc.compression); err != nil {
		return fmt.Errorf("configure remote (phase 1): %w", err)
	}

	// Poll until remote is in sync, calling override on every tick.
	deadline := time.Now().Add(initialSyncTimeout)
	ticker := time.NewTicker(syncthingProgressInterval)
	defer ticker.Stop()
	for {
		// Force local state onto remote.
		if err := syncthingOverrideFolder(ctx, localBase, localKey, folderID); err != nil {
			slog.Debug("syncthing override failed", "error", err)
		}

		need, err := syncthingFolderNeedBytes(ctx, remoteBase, remoteKey, folderID)
		if err != nil {
			slog.Debug("syncthing initial sync poll failed", "error", err)
		} else {
			fmt.Fprintf(out, "  %s\n", formatTwoPhaseNeedProgress("initial sync", need))
			if need == 0 {
				localConnected, localErr := syncthingPeerConnected(ctx, localBase, localKey, remoteID)
				remoteConnected, remoteErr := syncthingPeerConnected(ctx, remoteBase, remoteKey, localID)
				if localErr != nil {
					slog.Debug("syncthing local connection poll failed", "error", localErr)
				}
				if remoteErr != nil {
					slog.Debug("syncthing remote connection poll failed", "error", remoteErr)
				}
				if localConnected && remoteConnected {
					break
				}
				fmt.Fprintln(out, "  waiting for peer connection …")
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("initial sync did not complete within %s", initialSyncTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	// Phase 1b: flip ignoreDelete off and override so that files deleted
	// locally (e.g. after a branch switch) are propagated as deletions to
	// the remote before we enter bidirectional mode. Wait for convergence
	// just like phase 1.
	fmt.Fprintln(out, "Phase 1b: propagating local deletions …")
	if err := configureSyncthingPeer(ctx, localBase, localKey, localID, remoteID, peerAddr, folderID, localPath, "sendonly", sc.rescanInterval, sc.watcherDelay, false, sc.relays, sc.compression); err != nil {
		return fmt.Errorf("configure local (phase 1b): %w", err)
	}
	for {
		if err := syncthingOverrideFolder(ctx, localBase, localKey, folderID); err != nil {
			slog.Debug("syncthing override (phase 1b) failed", "error", err)
		}
		need, err := syncthingFolderNeedBytes(ctx, remoteBase, remoteKey, folderID)
		if err != nil {
			slog.Debug("syncthing phase 1b poll failed", "error", err)
		} else {
			fmt.Fprintf(out, "  %s\n", formatTwoPhaseNeedProgress("deletion propagation", need))
			if need == 0 {
				break
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("deletion propagation did not complete within %s", initialSyncTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}

	// Phase 2: switch to sendreceive on both sides.
	fmt.Fprintln(out, "Phase 2: switching to bidirectional sync …")
	if err := configureSyncthingPeer(ctx, localBase, localKey, localID, remoteID, peerAddr, folderID, localPath, "sendreceive", sc.rescanInterval, sc.watcherDelay, false, sc.relays, sc.compression); err != nil {
		return fmt.Errorf("configure local (phase 2): %w", err)
	}
	if err := configureSyncthingPeer(ctx, remoteBase, remoteKey, remoteID, localID, "", folderID, remotePath, "sendreceive", sc.rescanInterval, sc.watcherDelay, false, sc.relays, sc.compression); err != nil {
		return fmt.Errorf("configure remote (phase 2): %w", err)
	}

	localRestartRequired, err := syncthingRestartRequired(ctx, localBase, localKey)
	if err != nil {
		return fmt.Errorf("check local restart requirement: %w", err)
	}
	remoteRestartRequired, err := syncthingRestartRequired(ctx, remoteBase, remoteKey)
	if err != nil {
		return fmt.Errorf("check remote restart requirement: %w", err)
	}

	// The local daemon is launched with --no-restart so it can be owned by
	// the okdev sync child. Folder config changes are expected to apply live.
	// If Syncthing ever reports a restart requirement here, fail clearly so
	// we can handle that path explicitly rather than killing the daemon.
	if localRestartRequired {
		return errors.New("local syncthing reported restart-required after phase 2 config update")
	}
	if remoteRestartRequired {
		if err := syncthingRestart(ctx, remoteBase, remoteKey); err != nil {
			return fmt.Errorf("restart remote syncthing: %w", err)
		}
	}

	// Confirm the local API remained reachable through the live config update,
	// and wait for the remote API to come back only if it restarted.
	if err := waitSyncthingAPI(ctx, localBase, localKey, syncthingAPIReadyTimeout); err != nil {
		return fmt.Errorf("local syncthing not ready after phase 2 update: %w", err)
	}
	if remoteRestartRequired {
		if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
			return fmt.Errorf("remote syncthing not ready after restart: %w", err)
		}
	}
	if connected, err := syncthingPeerConnected(ctx, localBase, localKey, remoteID); err != nil {
		return fmt.Errorf("check local syncthing connection after restart: %w", err)
	} else if !connected {
		return errors.New("local syncthing peer not connected after restart")
	}
	if connected, err := syncthingPeerConnected(ctx, remoteBase, remoteKey, localID); err != nil {
		return fmt.Errorf("check remote syncthing connection after restart: %w", err)
	} else if !connected {
		return errors.New("remote syncthing peer not connected after restart")
	}

	fmt.Fprintln(out, "Two-phase initial sync complete, now in bidirectional mode.")
	return nil
}

// syncthingOverrideFolder calls POST /rest/db/override to force the local
// index as canonical for the given folder, causing diverged remote files to
// be overwritten.
func syncthingOverrideFolder(ctx context.Context, base, key, folderID string) error {
	path := fmt.Sprintf("/rest/db/override?folder=%s", url.QueryEscape(folderID))
	return syncthingPost(ctx, base, key, path, nil)
}

// syncthingRestart calls POST /rest/system/restart to restart a Syncthing
// instance, which is required to apply folder type changes.
func syncthingRestart(ctx context.Context, base, key string) error {
	return syncthingPost(ctx, base, key, "/rest/system/restart", nil)
}

func syncthingRestartRequired(ctx context.Context, base, key string) (bool, error) {
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, "/rest/config/restart-required", nil, "")
	if err != nil {
		return false, err
	}
	var payload struct {
		RequiresRestart bool `json:"requiresRestart"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		return false, err
	}
	return payload.RequiresRestart, nil
}

func folderTypesForMode(mode string) (local, remote string) {
	switch mode {
	case "up":
		return "sendonly", "receiveonly"
	case "down":
		return "receiveonly", "sendonly"
	default:
		return "sendreceive", "sendreceive"
	}
}

func configureSyncthingPeer(ctx context.Context, base, key, selfID, peerID, peerAddr, folderID, folderPath, folderType string, rescanIntervalSeconds, watcherDelaySeconds int, ignoreDelete, relaysEnabled, compression bool) error {
	cfg, err := syncthingGetConfig(ctx, base, key)
	if err != nil {
		return err
	}
	applyManagedSyncthingGlobalDefaults(cfg, relaysEnabled)

	compressionMode := "metadata"
	if compression {
		compressionMode = "always"
	}

	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		return err
	}
	addresses := syncthingDeviceAddresses(peerAddr)
	foundDevice := false
	for i, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			return err
		}
		if asString(m["deviceID"]) == peerID {
			m["addresses"] = addresses
			m["compression"] = compressionMode
			applyManagedSyncthingDeviceDefaults(m)
			devices[i] = m
			foundDevice = true
			break
		}
	}
	if !foundDevice {
		device := map[string]any{
			"deviceID":    peerID,
			"name":        "okdev-peer",
			"addresses":   addresses,
			"compression": compressionMode,
		}
		applyManagedSyncthingDeviceDefaults(device)
		devices = append(devices, device)
	}
	cfg["devices"] = devices

	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		return err
	}
	filteredFolders := make([]any, 0, len(folders))
	foundFolder := false
	for _, f := range folders {
		fm, err := syncthingObjectMap(f, "folders")
		if err != nil {
			return err
		}
		if asString(fm["id"]) == "default" {
			continue
		}
		if asString(fm["id"]) == folderID {
			fm["path"] = folderPath
			fm["type"] = folderType
			fm["markerName"] = "."
			fm["devices"] = []any{
				map[string]any{"deviceID": selfID},
				map[string]any{"deviceID": peerID},
			}
			applyManagedSyncthingFolderDefaults(fm, rescanIntervalSeconds, watcherDelaySeconds, ignoreDelete)
			filteredFolders = append(filteredFolders, fm)
			foundFolder = true
			continue
		}
		filteredFolders = append(filteredFolders, fm)
	}
	if !foundFolder {
		folder := map[string]any{
			"id":         folderID,
			"label":      folderID,
			"path":       folderPath,
			"type":       folderType,
			"markerName": ".",
			"devices": []any{
				map[string]any{"deviceID": selfID},
				map[string]any{"deviceID": peerID},
			},
		}
		applyManagedSyncthingFolderDefaults(folder, rescanIntervalSeconds, watcherDelaySeconds, ignoreDelete)
		filteredFolders = append(filteredFolders, folder)
	}
	cfg["folders"] = filteredFolders

	if err := syncthingSetConfig(ctx, base, key, cfg); err != nil {
		return err
	}
	return nil
}

// updateSyncthingDeviceAddress updates only the peer address for an existing
// device in the local Syncthing config. This is used during self-healing to
// point Syncthing at a new port-forward endpoint without touching folder or
// other device settings.
func updateSyncthingDeviceAddress(ctx context.Context, base, key, deviceID, newAddr string) error {
	cfg, err := syncthingGetConfig(ctx, base, key)
	if err != nil {
		return fmt.Errorf("get syncthing config: %w", err)
	}
	devices, err := syncthingObjectArray(cfg, "devices")
	if err != nil {
		return err
	}
	found := false
	for i, d := range devices {
		m, err := syncthingObjectMap(d, "devices")
		if err != nil {
			return err
		}
		if asString(m["deviceID"]) == deviceID {
			m["addresses"] = syncthingDeviceAddresses(newAddr)
			devices[i] = m
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("device %s not found in syncthing config", deviceID)
	}
	cfg["devices"] = devices
	return syncthingSetConfig(ctx, base, key, cfg)
}

func syncthingDeviceAddresses(peerAddr string) []any {
	if strings.TrimSpace(peerAddr) == "" {
		return []any{"dynamic"}
	}
	return []any{peerAddr}
}

func applyManagedSyncthingFolderDefaults(folder map[string]any, rescanIntervalSeconds, watcherDelaySeconds int, ignoreDelete bool) {
	delay := watcherDelaySeconds
	if delay <= 0 {
		delay = syncthingWatcherDelayS
	}
	folder["filesystemType"] = "basic"
	folder["fsWatcherEnabled"] = true
	folder["fsWatcherDelayS"] = delay
	folder["rescanIntervalS"] = rescanIntervalSeconds
	folder["maxConflicts"] = 0
	folder["ignoreDelete"] = ignoreDelete
	folder["order"] = "random"
	folder["scanProgressIntervalS"] = 1
	folder["pullerPauseS"] = 0
	folder["disableSparseFiles"] = false
	folder["disableTempIndexes"] = false
	folder["paused"] = false
	folder["weakHashThresholdPct"] = 25
	folder["markerName"] = "."
	folder["useLargeBlocks"] = false
	folder["copyRangeMethod"] = "all"
}

func applyManagedSyncthingGlobalDefaults(cfg map[string]any, relaysEnabled bool) {
	options, err := syncthingObjectMap(cfg["options"], "options")
	if err != nil {
		options = map[string]any{}
	}
	options["autoUpgradeIntervalH"] = 0
	options["upgradeToPreReleases"] = false
	options["urAccepted"] = -1
	options["relaysEnabled"] = relaysEnabled
	options["globalAnnounceEnabled"] = false
	options["localAnnounceEnabled"] = false
	options["natEnabled"] = false
	options["reconnectionIntervalS"] = 1
	options["startBrowser"] = false
	options["restartOnWakeup"] = true
	options["keepTemporariesH"] = 24
	options["cacheIgnoredFiles"] = false
	options["progressUpdateIntervalS"] = 1
	options["limitBandwidthInLan"] = false
	options["releasesURL"] = ""
	options["overwriteRemoteDeviceNamesOnConnect"] = false
	options["tempIndexMinBlocks"] = 10
	options["trafficClass"] = 0
	options["setLowPriority"] = false
	options["crashReportingEnabled"] = false
	cfg["options"] = options
}

func applyManagedSyncthingDeviceDefaults(device map[string]any) {
	device["paused"] = false
	device["autoAcceptFolders"] = false
	device["maxSendKbps"] = 0
	device["maxRecvKbps"] = 0
	device["maxRequestKiB"] = 0
}

func syncthingGetConfig(ctx context.Context, base, key string) (map[string]any, error) {
	cfg := map[string]any{}
	body, err := syncthingAPIRequestWithContext(ctx, http.MethodGet, base, key, "/rest/config", nil, "")
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(body, &cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

func syncthingManagedFolderType(cfg map[string]any, folderID string) (string, bool, error) {
	folders, err := syncthingObjectArray(cfg, "folders")
	if err != nil {
		return "", false, err
	}
	for _, f := range folders {
		fm, err := syncthingObjectMap(f, "folder entry")
		if err != nil {
			return "", false, err
		}
		if asString(fm["id"]) == folderID {
			return asString(fm["type"]), true, nil
		}
	}
	return "", false, nil
}

// shouldResumeTwoPhaseBootstrap returns true when the remote folder config
// indicates that a previous two-phase bootstrap did not finish.  A missing
// folder or a folder still set to a unidirectional type (sendonly/receiveonly)
// means phase 2 (sendreceive) was never reached.
//
// Callers must gate invocation on hasConfigState so that a brand-new session
// (where the folder simply hasn't been created yet) does not falsely trigger
// a two-phase resume.
func shouldResumeTwoPhaseBootstrap(folderType string, folderFound bool) bool {
	if !folderFound {
		return true
	}
	return strings.TrimSpace(folderType) != "sendreceive"
}

// remoteSyncthingBootstrapIncomplete checks whether a previous two-phase
// bootstrap did not complete by inspecting the remote folder type.  It opens
// its own port-forward; prefer passing bootstrapResume=true to
// waitForInitialSync instead to share the port-forward.
func remoteSyncthingBootstrapIncomplete(ctx context.Context, opts *Options, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod, sessionName string) (bool, error) {
	folderID := "okdev-" + sessionName
	remoteKey, err := readRemoteSyncthingAPIKey(ctx, k, namespace, pod)
	if err != nil {
		return false, fmt.Errorf("read remote syncthing API key: %w", err)
	}
	cancelPF, remoteBase, _, err := startSyncthingPortForward(ctx, opts, namespace, pod)
	if err != nil {
		return false, fmt.Errorf("port-forward to syncthing sidecar: %w", err)
	}
	defer cancelPF()
	if err := waitSyncthingAPI(ctx, remoteBase, remoteKey, syncthingAPIReadyTimeout); err != nil {
		return false, fmt.Errorf("remote syncthing not ready: %w", err)
	}
	cfg, err := syncthingGetConfig(ctx, remoteBase, remoteKey)
	if err != nil {
		return false, fmt.Errorf("read remote syncthing config: %w", err)
	}
	folderType, found, err := syncthingManagedFolderType(cfg, folderID)
	if err != nil {
		return false, fmt.Errorf("inspect remote syncthing folder %s: %w", folderID, err)
	}
	return shouldResumeTwoPhaseBootstrap(folderType, found), nil
}

func syncthingSetConfig(ctx context.Context, base, key string, cfg map[string]any) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	_, err = syncthingAPIRequestWithContext(ctx, http.MethodPut, base, key, "/rest/config", b, "application/json")
	if err != nil {
		return err
	}
	return nil
}

func syncthingPost(ctx context.Context, base, key, path string, body []byte) error {
	_, err := syncthingAPIRequestWithContext(ctx, http.MethodPost, base, key, path, body, "")
	if err != nil {
		return err
	}
	return nil
}

func syncthingAPIRequestWithContext(ctx context.Context, method, base, key, path string, body []byte, contentType string) ([]byte, error) {
	var reqBody io.Reader
	if len(body) > 0 {
		reqBody = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, base+path, reqBody)
	if err != nil {
		return nil, err
	}
	req.Header.Set("X-API-Key", key)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := syncthingHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	respBody, readErr := io.ReadAll(resp.Body)
	if readErr != nil {
		return nil, readErr
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("status %s", resp.Status)
	}
	return respBody, nil
}

func syncthingObjectArray(cfg map[string]any, key string) ([]any, error) {
	raw, ok := cfg[key]
	if !ok || raw == nil {
		return []any{}, nil
	}
	arr, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("syncthing config %q must be an array", key)
	}
	return arr, nil
}

func syncthingObjectMap(v any, field string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("syncthing config %q entries must be objects", field)
	}
	return m, nil
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}

// resetRemoteWorkspace clears the managed remote workspace subtree while
// preserving selected paths. The algorithm:
//  1. Move each preserve path to a temp location under the workspace root.
//  2. Remove everything else under the workspace root.
//  3. Move preserved paths back.
func resetRemoteWorkspace(ctx context.Context, k interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}, namespace, pod, remotePath string, preservePaths []string) error {
	remotePath = strings.TrimRight(remotePath, "/")
	if remotePath == "" || remotePath == "/" {
		return fmt.Errorf("refusing to reset root path")
	}

	// Validate preserve paths to prevent path traversal outside workspace.
	for _, p := range preservePaths {
		cleaned := filepath.Clean(p)
		if filepath.IsAbs(cleaned) || strings.HasPrefix(cleaned, "..") || strings.Contains(cleaned, string(filepath.Separator)+"..") {
			return fmt.Errorf("preservePath %q escapes the workspace root", p)
		}
	}

	esc := syncengine.ShellEscape
	tmpDir := remotePath + "/.okdev-reset-preserve"

	var script strings.Builder

	// Move preserved paths to temp.
	if len(preservePaths) > 0 {
		fmt.Fprintf(&script, "mkdir -p %s\n", esc(tmpDir))
		for _, p := range preservePaths {
			p = strings.Trim(p, "/")
			if p == "" || p == "." || p == ".." {
				continue
			}
			src := remotePath + "/" + p
			dst := tmpDir + "/" + p
			// Create parent directory for nested paths.
			dstParent := dst[:strings.LastIndex(dst, "/")]
			fmt.Fprintf(&script, "if [ -e %s ]; then mkdir -p %s && mv %s %s; fi\n", esc(src), esc(dstParent), esc(src), esc(dst))
		}
	}

	// Clear workspace contents (but not the directory itself).
	fmt.Fprintf(&script, "find %s -mindepth 1 -maxdepth 1 ! -name .okdev-reset-preserve -exec rm -rf {} +\n", esc(remotePath))

	// Restore preserved paths.
	if len(preservePaths) > 0 {
		for _, p := range preservePaths {
			p = strings.Trim(p, "/")
			if p == "" || p == "." || p == ".." {
				continue
			}
			src := tmpDir + "/" + p
			dst := remotePath + "/" + p
			dstParent := dst[:strings.LastIndex(dst, "/")]
			fmt.Fprintf(&script, "if [ -e %s ]; then mkdir -p %s && mv %s %s; fi\n", esc(src), esc(dstParent), esc(src), esc(dst))
		}
		fmt.Fprintf(&script, "rm -rf %s\n", esc(tmpDir))
	}

	_, err := execInSyncthingContainer(ctx, k, namespace, pod, script.String())
	return err
}

type syncthingSessionConfigState struct {
	Engine                string `json:"engine"`
	LocalPath             string `json:"localPath"`
	RemotePath            string `json:"remotePath"`
	WatcherDelaySeconds   int    `json:"watcherDelaySeconds,omitempty"`
	RescanIntervalSeconds int    `json:"rescanIntervalSeconds,omitempty"`
	RelaysEnabled         bool   `json:"relaysEnabled,omitempty"`
	Compression           bool   `json:"compression,omitempty"`
}

func syncthingSessionConfigHash(cfg *config.DevEnvironment, localPath, remotePath string) string {
	if cfg == nil {
		return ""
	}
	state := syncthingSessionConfigState{
		Engine:                strings.TrimSpace(cfg.Spec.Sync.Engine),
		LocalPath:             localPath,
		RemotePath:            remotePath,
		WatcherDelaySeconds:   cfg.Spec.Sync.Syncthing.WatcherDelaySeconds,
		RescanIntervalSeconds: cfg.Spec.Sync.Syncthing.RescanIntervalSeconds,
		RelaysEnabled:         cfg.Spec.Sync.Syncthing.RelaysEnabled,
		Compression:           cfg.Spec.Sync.Syncthing.Compression,
	}
	b, err := json.Marshal(state)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(b)
	return fmt.Sprintf("%x", sum[:])
}

type syncLargeFile struct {
	Path string
	Size int64
}

type syncLargeDir struct {
	Path string
	Size int64
}

func warnLargeSyncEntries(w io.Writer, localPath string, needBytes int64) {
	files, dirs, err := detectLargeSyncEntries(localPath, largeSyncWarnThreshold, largeSyncWarnLimit)
	if err != nil || (len(files) == 0 && len(dirs) == 0) {
		return
	}
	fmt.Fprintf(w, "warning: sync still needs %s; largest local files and folders under %s:\n", humanSizeIEC(needBytes), localPath)
	for _, f := range files {
		fmt.Fprintf(w, "- file %s  %s\n", humanSizeIEC(f.Size), f.Path)
	}
	for _, d := range dirs {
		fmt.Fprintf(w, "- dir  %s  %s/\n", humanSizeIEC(d.Size), d.Path)
	}
	fmt.Fprintln(w, "consider adding large generated or dataset files to .stignore")
}

func detectLargeSyncEntries(root string, threshold int64, limit int) ([]syncLargeFile, []syncLargeDir, error) {
	ignores, err := loadSTIgnorePatterns(root)
	if err != nil {
		return nil, nil, err
	}
	var files []syncLargeFile
	dirSizes := map[string]int64{}
	err = filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == root {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if matchesSTIgnore(rel, d.IsDir(), ignores) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		if info.Size() >= threshold {
			files = append(files, syncLargeFile{Path: rel, Size: info.Size()})
		}
		if slash := strings.IndexByte(rel, '/'); slash > 0 {
			dirSizes[rel[:slash]] += info.Size()
		}
		return nil
	})
	if err != nil {
		return nil, nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Size > files[j].Size })
	if len(files) > limit {
		files = files[:limit]
	}
	var dirs []syncLargeDir
	for path, size := range dirSizes {
		if size >= threshold {
			dirs = append(dirs, syncLargeDir{Path: path, Size: size})
		}
	}
	sort.Slice(dirs, func(i, j int) bool { return dirs[i].Size > dirs[j].Size })
	if len(dirs) > limit {
		dirs = dirs[:limit]
	}
	return files, dirs, nil
}

func loadSTIgnorePatterns(root string) ([]string, error) {
	content, err := os.ReadFile(filepath.Join(root, ".stignore"))
	if err != nil {
		if os.IsNotExist(err) {
			return defaultSyncExcludes, nil
		}
		return nil, err
	}
	var patterns []string
	for _, line := range strings.Split(string(content), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "//") || strings.HasPrefix(line, "!") {
			continue
		}
		patterns = append(patterns, line)
	}
	return patterns, nil
}

func matchesSTIgnore(rel string, isDir bool, patterns []string) bool {
	base := path.Base(rel)
	for _, raw := range patterns {
		p := strings.TrimSpace(filepath.ToSlash(raw))
		if p == "" {
			continue
		}
		dirOnly := strings.HasSuffix(p, "/")
		if dirOnly {
			p = strings.TrimSuffix(p, "/")
			if !isDir {
				if rel == p || strings.HasPrefix(rel, p+"/") {
					return true
				}
				continue
			}
			if rel == p || strings.HasPrefix(rel, p+"/") || base == p {
				return true
			}
			continue
		}
		if strings.HasPrefix(p, "/") {
			p = strings.TrimPrefix(p, "/")
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			continue
		}
		if strings.ContainsAny(p, "*?[") {
			if ok, _ := path.Match(p, rel); ok {
				return true
			}
			if ok, _ := path.Match(p, base); ok {
				return true
			}
			continue
		}
		if rel == p || strings.HasPrefix(rel, p+"/") || base == p {
			return true
		}
	}
	return false
}

func humanSizeIEC(size int64) string {
	const unit = 1024
	if size < unit {
		return fmt.Sprintf("%dB", size)
	}
	div, exp := int64(unit), 0
	for n := size / unit; n >= unit; n /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f%ciB", float64(size)/float64(div), "KMGTPE"[exp])
}

// syncHealthStatus represents the sync health as checked by the CLI.
type syncHealthStatus string

const (
	syncHealthActive  syncHealthStatus = "active"
	syncHealthStale   syncHealthStatus = "stale"
	syncHealthStopped syncHealthStatus = "stopped"
)

// checkSyncHealth checks the health of the Syncthing sync for a session.
// It checks whether the process is alive and optionally queries the local
// Syncthing API for connection and folder status.
func checkSyncHealth(sessionName string) (syncHealthStatus, string) {
	pidPath, err := syncthingPIDStatusPath(sessionName)
	if err != nil {
		return syncHealthStopped, "sync is not running"
	}
	pid, ok := readSyncthingPID(pidPath)
	if !ok || !processAlive(pid) || !processLooksLikeSyncthingSync(pid) {
		return syncHealthStopped, "sync is not running"
	}

	// Process is alive — try to query the local Syncthing API.
	// Use the read-only home path to avoid creating directories.
	home, err := localSyncthingStatusHome(sessionName)
	if err != nil {
		return syncHealthActive, ""
	}
	apiBase, apiKey, err := readLocalSyncthingEndpoint(home)
	if err != nil {
		// Can't read config but process is alive, treat as active.
		return syncHealthActive, ""
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	// Check connections to see if peer is connected.
	connected, err := syncthingPeerConnected(ctx, apiBase, apiKey, "")
	if err != nil {
		// API unreachable likely means Syncthing is still starting up or
		// restarting (e.g. during two-phase transition). Treat as active
		// rather than stale since the process is running.
		return syncHealthActive, ""
	}
	if !connected {
		return syncHealthStale, "peer disconnected"
	}

	return syncHealthActive, ""
}

// readLocalSyncthingEndpoint reads the API base URL and key from a local
// Syncthing home directory's config.xml.
func readLocalSyncthingEndpoint(home string) (apiBase, apiKey string, err error) {
	if endpointPath, pathErr := localSyncthingEndpointPath(home); pathErr == nil {
		if b, readErr := os.ReadFile(endpointPath); readErr == nil {
			apiBase = strings.TrimSpace(string(b))
		}
	}
	paths := []string{filepath.Join(home, "config.xml"), filepath.Join(home, "config", "config.xml")}
	for _, p := range paths {
		b, readErr := os.ReadFile(p)
		if readErr != nil {
			continue
		}
		var cfg syncthingConfigXML
		if xmlErr := xml.Unmarshal(b, &cfg); xmlErr != nil {
			continue
		}
		if cfg.GUI.APIKey == "" || cfg.GUI.Address == "" {
			continue
		}
		if apiBase == "" {
			addr := cfg.GUI.Address
			if !strings.HasPrefix(addr, "http") {
				addr = "http://" + addr
			}
			apiBase = addr
		}
		return apiBase, cfg.GUI.APIKey, nil
	}
	return "", "", errors.New("unable to read local syncthing endpoint")
}

// warnSyncHealth prints a sync health warning to w if the sync for the
// given session is unhealthy. Returns the health status.
func warnSyncHealth(w io.Writer, sessionName string) syncHealthStatus {
	status, reason := checkSyncHealth(sessionName)
	switch status {
	case syncHealthStopped:
		fmt.Fprintf(w, "warning: sync is not running — run \"okdev sync\" to restart, or \"okdev sync reset-remote\" to reset workspace\n")
	case syncHealthStale:
		fmt.Fprintf(w, "warning: sync may be stale — %s — run \"okdev sync --reset\" to re-establish sync\n", reason)
	}
	return status
}
