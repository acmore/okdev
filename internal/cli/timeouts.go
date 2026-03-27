package cli

import "time"

// Centralized timeout and interval constants used across CLI commands.
// Keeping these in one place makes tuning easier and prevents silent
// divergence when the same logical timeout is expressed as a different
// literal in multiple files.

const (
	// Session heartbeat interval for activity annotation updates.
	sessionHeartbeatInterval = 5 * time.Minute

	// Default context timeout for short-lived Kubernetes operations.
	defaultContextTimeout = 5 * time.Minute

	// Default --wait-timeout for `okdev up`.
	upDefaultWaitTimeout = 10 * time.Minute

	// Extra headroom added around up wait timeout when building the parent context.
	upContextBuffer = 2 * time.Minute

	// Timeout for session-access and ownership checks.
	sessionAccessTimeout = 15 * time.Second

	// Timeout for session-existence probes (ListPods / GetPodSummary).
	sessionExistsTimeout = 10 * time.Second

	// Timeout for heartbeat annotation writes.
	heartbeatWriteTimeout = 10 * time.Second

	// Timeout for a single kubectl exec probe (sshd readiness, tmux detect, etc).
	execProbeTimeout = 10 * time.Second

	// Timeout for SSH key installation retry loop.
	sshKeyInstallTimeout = 30 * time.Second

	// Timeout when waiting for SSH service during setup/recovery flows.
	sshServiceWaitTimeout = 20 * time.Second

	// Timeout for ensureDevSSHDRunning exec.
	sshdStartTimeout = 30 * time.Second

	// Timeout for postCreate command execution.
	postCreateTimeout = 30 * time.Minute

	// Timeout for annotation read/write during postCreate.
	annotationTimeout = 10 * time.Second

	// Timeout for config drift warning check.
	configDriftTimeout = 10 * time.Second

	// Port-forward readiness poll interval.
	portForwardPollInterval = 200 * time.Millisecond

	// Port-forward readiness timeout.
	portForwardReadinessTimeout = 10 * time.Second

	// Port-forward keepalive probe interval (used in ssh and ssh-proxy).
	portForwardKeepaliveInterval = 10 * time.Second

	// Port-forward shutdown grace period.
	portForwardShutdownWait = 2 * time.Second

	// No-probe port-forward startup grace period.
	portForwardNoProbeGrace = 500 * time.Millisecond

	// Dial timeout for local port-forward probes.
	portForwardDialTimeout = 200 * time.Millisecond

	// Dial timeout for port-forward keepalive probes.
	portForwardKeepaliveDialTimeout = 2 * time.Second

	// Dial timeout for waitDialLocal.
	localDialTimeout = 750 * time.Millisecond

	// SSH keepalive RTT warning threshold.
	sshRTTWarnThreshold = 5 * time.Second

	// Minimum interval between SSH RTT warnings.
	sshRTTWarnCooldown = 30 * time.Second

	// SSH session readiness probe timeout per attempt.
	sshReadinessProbeTimeout = 2 * time.Second

	// SSH reconnect wait timeout per cycle.
	sshReconnectWaitTimeout = 15 * time.Second

	// SSH session readiness wait timeout.
	sshReadinessTimeout = 15 * time.Second

	// Maximum total time spent trying to recover an interactive SSH shell.
	sshRecoveryTimeout = 3 * time.Minute

	// Window used to classify repeated fast SSH shell failures as unrecoverable.
	sshRapidFailureWindow = 60 * time.Second

	// Threshold below which an SSH shell exit counts as a rapid failure.
	sshShellStartThreshold = 5 * time.Second

	// Backoff step between SSH key installation retries.
	sshKeyInstallRetryStep = 500 * time.Millisecond

	// Transient status spinner frame interval.
	statusSpinnerInterval = 120 * time.Millisecond

	// Default SSH port in dev containers.
	sshPort = 2222

	// Syncthing watcher delay (seconds).
	syncthingWatcherDelayS = 10

	// TCP keepalive period for proxy connections.
	proxyTCPKeepAlivePeriod = 10 * time.Second

	// Data-flow watchdog check interval.
	dataFlowWatchdogInterval = 5 * time.Second

	// Data-flow watchdog idle timeout.
	dataFlowWatchdogIdleTimeout = 15 * time.Second

	// TCP keepalive period for proxy sockets.
	proxySocketKeepAlivePeriod = 5 * time.Second

	// SSHD readiness poll interval.
	sshdReadinessPollInterval = 300 * time.Millisecond

	// SSH readiness success interval between consecutive probes.
	sshReadinessSuccessWait = 150 * time.Millisecond

	// SSH readiness failure retry interval.
	sshReadinessFailureWait = 250 * time.Millisecond

	// waitDialLocal retry interval.
	localDialRetryInterval = 150 * time.Millisecond

	// waitDialLocal timeout.
	localDialTotalTimeout = 10 * time.Second

	// Tmux installation progress update interval.
	tmuxInstallProgressInterval = 10 * time.Second

	// Timeout for validating a pinned interactive target pod.
	targetValidationTimeout = 15 * time.Second

	// Timeout for local Syncthing binary bootstrap and sidecar setup.
	syncthingBootstrapTimeout = 2 * time.Minute

	// HTTP timeout for Syncthing REST API requests.
	syncthingHTTPClientTimeout = 15 * time.Second

	// Max wait for Syncthing REST API readiness.
	syncthingAPIReadyTimeout = 30 * time.Second

	// Poll interval while waiting for the Syncthing API to come up.
	syncthingAPIReadyPollInterval = 1 * time.Second

	// Progress reporting interval for foreground Syncthing sync.
	syncthingProgressInterval = 5 * time.Second

	// Delay used to confirm a detached Syncthing child stayed up.
	syncthingDetachedStartupDelay = 300 * time.Millisecond

	// Grace period for Syncthing child shutdown after SIGTERM.
	syncthingShutdownWait = 3 * time.Second

	// Poll interval while waiting for Syncthing child shutdown.
	syncthingShutdownPollInterval = 100 * time.Millisecond
)
