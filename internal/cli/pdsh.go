package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/shellutil"
	"github.com/spf13/cobra"
	k8sexec "k8s.io/client-go/util/exec"
)

const detachMetadataDir = "/tmp/okdev-exec"
const detachPIDPlaceholder = "__OKDEV_DETACH_PID__"
const detachExitPlaceholder = "__OKDEV_DETACH_EXIT__"

func newExecCmd(opts *Options) *cobra.Command {
	var shell string
	var noTTY bool
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var container string
	var detach bool
	var timeout time.Duration
	var logDir string
	var noPrefix bool
	var fanout int
	var readyOnly bool

	cmd := &cobra.Command{
		Use:   "exec [session]",
		Short: "Open shell or run command in session pod(s)",
		Long:  "Open a shell via kubectl exec, or run a command across multiple pods in parallel (pdsh-style).\nFor persistent tmux sessions and port forwarding, use okdev ssh instead.",
		Example: `  # Open an interactive shell
  okdev exec

  # Run a command on all session pods
  okdev exec -- nvidia-smi

  # Run on a specific role
  okdev exec --role worker -- df -h /workspace

  # Detach a background command on worker pods
  okdev exec --role worker --detach -- python train.py`,
		Aliases:           []string{"connect"},
		Args:              validateExecArgs,
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionArgs, commandArgs := splitExecArgs(cmd, args)
			applySessionArg(opts, sessionArgs)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}

			multiPod := len(commandArgs) > 0 || len(podNames) > 0 || role != "" || len(labels) > 0 || detach
			if err := validateMultiPodFlags(podNames, role, labels, exclude, len(commandArgs) > 0, detach); err != nil {
				return err
			}

			if multiPod || detach {
				return runMultiPodExec(cmd, cc, commandArgs, podNames, role, labels, exclude, container, detach, timeout, logDir, noPrefix, fanout, readyOnly)
			}

			// Single-pod mode (existing behavior)
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			if container != "" {
				target.Container = container
			}
			stopMaintenance := startSessionMaintenanceWithClient(cc.kube, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			execCmd := []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			if shell != "" {
				execCmd = []string{shell}
			}
			if len(execCmd) == 1 && strings.TrimSpace(execCmd[0]) == "" {
				execCmd = []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			}
			return runConnectWithClient(cc.kube, cc.namespace, target, execCmd, !noTTY)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to start (default auto-detects bash/sh)")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Disable TTY allocation")
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().BoolVar(&detach, "detach", false, "Launch command in background and return")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Per-pod command timeout (e.g., 30s, 5m)")
	cmd.Flags().StringVar(&logDir, "log-dir", "", "Write per-pod logs to directory")
	cmd.Flags().BoolVar(&noPrefix, "no-prefix", false, "Suppress pod name prefix in output")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod executions")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Run only on pods that are already running (skip readiness check)")
	return cmd
}

func splitExecArgs(cmd *cobra.Command, args []string) ([]string, []string) {
	dash := cmd.ArgsLenAtDash()
	if dash < 0 {
		return args, nil
	}
	return args[:dash], args[dash:]
}

func validateExecArgs(cmd *cobra.Command, args []string) error {
	sessionArgs, _ := splitExecArgs(cmd, args)
	if len(sessionArgs) > 1 {
		return fmt.Errorf("accepts at most 1 session argument before --")
	}
	return nil
}

func validateMultiPodFlags(podNames []string, role string, labels []string, exclude []string, hasCommand bool, detach bool) error {
	usesSelector := len(podNames) > 0 || role != "" || len(labels) > 0 || len(exclude) > 0
	if (usesSelector || detach) && !hasCommand {
		return fmt.Errorf("command after -- is required when using --pod, --role, --label, --exclude, or --detach")
	}
	if len(exclude) > 0 && len(podNames) > 0 {
		return fmt.Errorf("--exclude cannot be used with --pod")
	}
	if !hasCommand && !detach {
		return nil
	}
	exclusiveCount := 0
	if len(podNames) > 0 {
		exclusiveCount++
	}
	if role != "" {
		exclusiveCount++
	}
	if len(labels) > 0 {
		exclusiveCount++
	}
	if exclusiveCount > 1 {
		return fmt.Errorf("--pod, --role, and --label are mutually exclusive")
	}
	return nil
}

func runMultiPodExec(cmd *cobra.Command, cc *commandContext, commandArgs []string, podNames []string, role string, labels []string, exclude []string, container string, detach bool, timeout time.Duration, logDir string, noPrefix bool, fanout int, readyOnly bool) error {
	ctx := cmd.Context()
	pods, err := selectSessionPods(ctx, cc, podNames, role, labels, exclude, readyOnly)
	if err != nil {
		return err
	}

	targetContainer := container
	if targetContainer == "" {
		targetContainer = resolveTargetContainer(cc.cfg)
	}

	if logDir != "" {
		if err := os.MkdirAll(logDir, 0o755); err != nil {
			return fmt.Errorf("create log directory: %w", err)
		}
	}

	effectiveFanout := fanout
	if effectiveFanout <= 0 {
		effectiveFanout = pdshDefaultFanout
	}

	if detach {
		return runDetachExec(ctx, cc.kube, cc.namespace, pods, targetContainer, commandArgsShellString(commandArgs), effectiveFanout, cmd.OutOrStdout())
	}

	return runMultiExec(ctx, cc.kube, cc.namespace, pods, targetContainer, commandArgs, logDir, timeout, noPrefix, effectiveFanout, cmd.OutOrStdout(), cmd.ErrOrStderr())
}

// selectSessionPods lists the session's pods, applies user filters, and
// enforces the running-vs-targeted readiness check shared by exec and
// exec-jobs. The returned slice is the set of running pods that satisfy the
// filters; readyOnly skips the failure when some targeted pods are not yet
// running.
func selectSessionPods(ctx context.Context, cc *commandContext, podNames []string, role string, labels []string, exclude []string, readyOnly bool) ([]kube.PodSummary, error) {
	labelSel := selectorForSessionRun(cc.sessionName)
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return nil, fmt.Errorf("list session pods: %w", err)
	}

	// Apply user-specified filters before the readiness check so the
	// running-vs-total comparison only considers targeted pods.
	allPods := sessionPods
	switch {
	case len(podNames) > 0:
		allPods = filterPodsByName(allPods, podNames)
	case role != "":
		allPods = filterPodsByRole(allPods, role)
	case len(labels) > 0:
		allPods = filterPodsByLabels(allPods, labels)
	}
	if len(exclude) > 0 {
		allPods = excludePods(allPods, exclude)
	}
	if len(allPods) == 0 {
		return nil, fmt.Errorf("no pods match the specified filters in session %q", cc.sessionName)
	}
	pods := filterRunningPods(allPods)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no running pods in session %q (0/%d pods ready)", cc.sessionName, len(allPods))
	}
	if len(pods) < len(allPods) && !readyOnly {
		notReady := make([]string, 0, len(allPods)-len(pods))
		runningSet := make(map[string]bool, len(pods))
		for _, p := range pods {
			runningSet[p.Name] = true
		}
		for _, p := range allPods {
			if !runningSet[p.Name] {
				notReady = append(notReady, fmt.Sprintf("%s (%s)", p.Name, p.Phase))
			}
		}
		return nil, fmt.Errorf("%d/%d pods are not running: %s\nUse --ready-only to run on the %d ready pods",
			len(notReady), len(allPods), strings.Join(notReady, ", "), len(pods))
	}
	return pods, nil
}

func commandArgsShellString(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellutil.Quote(arg))
	}
	return strings.Join(parts, " ")
}

type podExecResult struct {
	pod string
	err error
}

func runMultiExec(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, command []string, logDir string, timeout time.Duration, noPrefix bool, fanout int, stdout, stderr io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)
	colorEnabled := isInteractiveWriter(stdout)
	displayPrefixes := formatPodPrefixes(shortNames, colorEnabled)

	var writeMu sync.Mutex
	noPrefixOut := &lockedWriter{w: stdout}
	noPrefixErr := &lockedWriter{w: stderr}
	results := make(chan podExecResult, len(pods))
	sem := make(chan struct{}, fanout)

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName, displayPrefix string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			execCtx := ctx
			var cancel context.CancelFunc
			if timeout > 0 {
				execCtx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			var podStdout, podStderr io.Writer
			if noPrefix {
				podStdout = noPrefixOut
				podStderr = noPrefixErr
			} else {
				pw := newPrefixedWriter(displayPrefix, stdout, &writeMu)
				pe := newPrefixedWriter(displayPrefix, stderr, &writeMu)
				defer pw.Flush()
				defer pe.Flush()
				podStdout = pw
				podStderr = pe
			}

			if logDir != "" {
				logPath := filepath.Join(logDir, shortName+".log")
				f, err := os.Create(logPath)
				if err != nil {
					results <- podExecResult{pod: pod.Name, err: fmt.Errorf("create log file: %w", err)}
					return
				}
				defer f.Close()
				podStdout = io.MultiWriter(podStdout, f)
				podStderr = io.MultiWriter(podStderr, f)
			}

			err := connect.RunOnContainer(execCtx, client, namespace, pod.Name, container, command, false, nil, podStdout, podStderr)
			results <- podExecResult{pod: pod.Name, err: err}
		}(pod, shortNames[i], displayPrefixes[i])
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	var failures []podExecResult
	for r := range results {
		if r.err != nil {
			failures = append(failures, r)
		}
	}
	if len(failures) > 0 {
		parts := make([]string, 0, len(failures))
		for _, f := range failures {
			parts = append(parts, formatPodExecFailureSummary(f.pod, container, f.err))
		}
		fmt.Fprintf(stderr, "\nFAILED:\n%s\n", strings.Join(parts, "\n"))
		return fmt.Errorf("%d of %d pods failed", len(failures), len(pods))
	}
	return nil
}

func formatPodExecFailureSummary(podName, container string, err error) string {
	containerLabel := container
	if strings.TrimSpace(containerLabel) == "" {
		containerLabel = "<default>"
	}
	kind, exitCode := classifyPodExecFailure(err)
	parts := []string{
		fmt.Sprintf("pod=%s", podName),
		fmt.Sprintf("container=%s", containerLabel),
		fmt.Sprintf("kind=%s", kind),
	}
	if exitCode >= 0 {
		parts = append(parts, fmt.Sprintf("exit=%d", exitCode))
	}
	if extra := failureMessageDetail(err, kind, exitCode); extra != "" {
		parts = append(parts, fmt.Sprintf("error=%q", extra))
	}
	return strings.Join(parts, " ")
}

// failureMessageDetail returns a one-line excerpt of err suitable for the
// failure summary, or empty string when err adds no signal beyond kind/exit.
// For remote-exit failures we suppress the boilerplate "command terminated
// with exit code N" message but keep any wrapping context.
func failureMessageDetail(err error, kind string, exitCode int) string {
	if err == nil {
		return ""
	}
	msg := strings.Join(strings.Fields(err.Error()), " ")
	if msg == "" {
		return ""
	}
	if kind != "remote-exit" {
		return msg
	}
	boilerplate := []string{
		fmt.Sprintf("command terminated with exit code %d", exitCode),
		fmt.Sprintf("exit code %d", exitCode),
		fmt.Sprintf("exit status %d", exitCode),
	}
	for _, b := range boilerplate {
		if strings.EqualFold(msg, b) {
			return ""
		}
	}
	return msg
}

func classifyPodExecFailure(err error) (kind string, exitCode int) {
	if err == nil {
		return "unknown", -1
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout", -1
	}
	var exitErr k8sexec.ExitError
	if errors.As(err, &exitErr) {
		return "remote-exit", exitErr.ExitStatus()
	}
	if code, ok := parseExecExitCode(strings.ToLower(err.Error())); ok {
		return "remote-exit", code
	}
	if connect.IsTransientExecError(err) {
		return "transport", -1
	}
	return "command-error", -1
}

func parseExecExitCode(msg string) (int, bool) {
	for _, marker := range []string{"exit code ", "exit status "} {
		idx := strings.LastIndex(msg, marker)
		if idx < 0 {
			continue
		}
		value := strings.TrimSpace(msg[idx+len(marker):])
		end := 0
		for end < len(value) && value[end] >= '0' && value[end] <= '9' {
			end++
		}
		if end == 0 {
			continue
		}
		code, err := strconv.Atoi(value[:end])
		if err == nil {
			return code, true
		}
	}
	return 0, false
}

type detachJobSpec struct {
	JobID      string
	Pod        string
	Container  string
	Command    string
	StartedAt  string
	LogPath    string
	MetaPath   string
	ScriptPath string
}

type detachMetadata struct {
	JobID      string `json:"jobId"`
	Pod        string `json:"pod"`
	Container  string `json:"container"`
	PID        int    `json:"pid"`
	Command    string `json:"command"`
	StartedAt  string `json:"startedAt"`
	StdoutPath string `json:"stdoutPath"`
	StderrPath string `json:"stderrPath"`
	MetaPath   string `json:"metaPath"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exitCode,omitempty"`
}

type detachLaunchInfo struct {
	JobID    string `json:"jobId"`
	PID      int    `json:"pid"`
	LogPath  string `json:"logPath"`
	MetaPath string `json:"metaPath"`
}

type podDetachResult struct {
	pod  string
	info detachLaunchInfo
	err  error
}

func detachCommand(spec detachJobSpec) []string {
	launchTemplate := mustDetachJSONTemplate(detachLaunchInfo{
		JobID:    spec.JobID,
		PID:      0,
		LogPath:  spec.LogPath,
		MetaPath: spec.MetaPath,
	})
	runningTemplate := mustDetachJSONTemplate(detachMetadata{
		JobID:      spec.JobID,
		Pod:        spec.Pod,
		Container:  spec.Container,
		PID:        0,
		Command:    spec.Command,
		StartedAt:  spec.StartedAt,
		StdoutPath: spec.LogPath,
		StderrPath: spec.LogPath,
		MetaPath:   spec.MetaPath,
		State:      "running",
	})
	exitCodeZero := 0
	completionTemplate := mustDetachJSONTemplateWithExit(detachMetadata{
		JobID:      spec.JobID,
		Pod:        spec.Pod,
		Container:  spec.Container,
		PID:        0,
		Command:    spec.Command,
		StartedAt:  spec.StartedAt,
		StdoutPath: spec.LogPath,
		StderrPath: spec.LogPath,
		MetaPath:   spec.MetaPath,
		State:      "exited",
		ExitCode:   &exitCodeZero,
	})
	wrapper := detachWrapperScript(spec.JobID, spec.Command, spec.MetaPath, runningTemplate, completionTemplate)
	script := fmt.Sprintf(
		"set -eu\n"+
			"log_path=%s\n"+
			"meta_path=%s\n"+
			"wrapper_path=%s\n"+
			"launch_json=%s\n"+
			"mkdir -p %s\n"+
			"cat >\"$wrapper_path\" <<'OKDEV_DETACH_WRAPPER'\n%s\nOKDEV_DETACH_WRAPPER\n"+
			"chmod 700 \"$wrapper_path\"\n"+
			"nohup sh \"$wrapper_path\" >\"$log_path\" 2>&1 &\n"+
			"wrapper_pid=$!\n"+
			"if [ -z \"$wrapper_pid\" ] || [ \"$wrapper_pid\" -le 0 ]; then\n"+
			"  echo 'failed to capture wrapper pid' >&2\n"+
			"  exit 1\n"+
			"fi\n"+
			// Wait for the wrapper to publish its initial metadata before we
			// hand the user a launch record. This avoids a race where the
			// caller sees a pid but a follow-up `okdev exec-jobs` finds no
			// metadata yet, and ensures the wrapper (not the launcher) owns
			// every state transition for $meta_path so a fast-exiting command
			// cannot have its 'exited' record clobbered by a later 'running'.
			"i=0\n"+
			"while [ ! -e \"$meta_path\" ] && [ \"$i\" -lt 50 ]; do\n"+
			"  sleep 0.1 2>/dev/null || sleep 1\n"+
			"  i=$((i+1))\n"+
			"done\n"+
			"if [ ! -e \"$meta_path\" ]; then\n"+
			"  echo 'detached job did not publish metadata in time' >&2\n"+
			"  exit 1\n"+
			"fi\n"+
			// Report the user command's pid (not the wrapper shell's) so
			// `kill <pid>` targets the right process. The wrapper writes the
			// real pid into the metadata before we unblock above.
			"pid=$(sed -n 's/.*\"pid\":\\([0-9][0-9]*\\).*/\\1/p' \"$meta_path\" | head -n1)\n"+
			"if [ -z \"$pid\" ]; then\n"+
			"  echo 'detached job metadata missing pid' >&2\n"+
			"  exit 1\n"+
			"fi\n"+
			"printf '%%s\\n' \"$launch_json\" | sed \"s/%s/$pid/g\"\n",
		shellutil.Quote(spec.LogPath),
		shellutil.Quote(spec.MetaPath),
		shellutil.Quote(spec.ScriptPath),
		shellutil.Quote(launchTemplate),
		shellutil.Quote(detachMetadataDir),
		wrapper,
		detachPIDPlaceholder,
	)
	return []string{"sh", "-c", script}
}

// detachWrapperScript builds the body of the wrapper script that runs in the
// background on the target container. The wrapper is the sole writer of the
// per-job metadata file, which eliminates the race between the launcher's
// "running" write and the wrapper's "exited" write.
//
// The user command is launched via `(exec CMD) &` so the backgrounded
// subshell immediately execs into CMD, making $! the pid of CMD itself (not
// a wrapper shell). This is what we record as the job's pid so a caller can
// `kill <pid>` or inspect /proc/<pid> and get the process they expect. We
// export OKDEV_JOB_ID first so exec-jobs can reconcile liveness via
// /proc/<pid>/environ, which is robust against PID reuse.
//
// We deliberately do NOT `set -e` around the user command so that a
// non-zero exit still falls through to the EXIT trap that records the real
// exit code.
func detachWrapperScript(jobID, command, metaPath, runningTemplate, completionTemplate string) string {
	return fmt.Sprintf(
		"meta_path=%s\n"+
			"meta_tmp=\"${meta_path}.tmp\"\n"+
			"running_json=%s\n"+
			"exit_json=%s\n"+
			"export OKDEV_JOB_ID=%s\n"+
			"(exec %s) &\n"+
			"user_pid=$!\n"+
			"if [ -z \"$user_pid\" ] || [ \"$user_pid\" -le 0 ]; then\n"+
			"  exit 1\n"+
			"fi\n"+
			"printf '%%s\\n' \"$running_json\" | sed \"s/%s/$user_pid/g\" >\"$meta_tmp\" || exit 1\n"+
			"mv \"$meta_tmp\" \"$meta_path\" || exit 1\n"+
			"trap 'rc=$?; printf \"%%s\\n\" \"$exit_json\" | sed \"s/%s/$user_pid/g; s/%s/$rc/g\" >\"$meta_tmp\" 2>/dev/null && mv \"$meta_tmp\" \"$meta_path\" 2>/dev/null; exit $rc' EXIT\n"+
			"wait \"$user_pid\"\n",
		shellutil.Quote(metaPath),
		shellutil.Quote(runningTemplate),
		shellutil.Quote(completionTemplate),
		shellutil.Quote(jobID),
		command,
		detachPIDPlaceholder,
		detachPIDPlaceholder,
		detachExitPlaceholder,
	)
}

func mustDetachJSONTemplate(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out := strings.Replace(string(data), `"pid":0`, `"pid":`+detachPIDPlaceholder, 1)
	return out
}

func mustDetachJSONTemplateWithExit(v detachMetadata) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	out := strings.Replace(string(data), `"pid":0`, `"pid":`+detachPIDPlaceholder, 1)
	out = strings.Replace(out, `"exitCode":0`, `"exitCode":`+detachExitPlaceholder, 1)
	return out
}

func newDetachJobSpec(pod, container, cmd string) detachJobSpec {
	jobID := newDetachJobID()
	startedAt := time.Now().UTC().Format(time.RFC3339)
	logPath := filepath.Join(detachMetadataDir, jobID+".log")
	metaPath := filepath.Join(detachMetadataDir, jobID+".json")
	scriptPath := filepath.Join(detachMetadataDir, jobID+".sh")
	return detachJobSpec{
		JobID:      jobID,
		Pod:        pod,
		Container:  container,
		Command:    cmd,
		StartedAt:  startedAt,
		LogPath:    logPath,
		MetaPath:   metaPath,
		ScriptPath: scriptPath,
	}
}

func newDetachJobID() string {
	var suffix [4]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		return time.Now().UTC().Format("20060102T150405Z")
	}
	return time.Now().UTC().Format("20060102T150405Z") + "-" + hex.EncodeToString(suffix[:])
}

func parseDetachLaunchOutput(raw string) (detachLaunchInfo, error) {
	lines := strings.Split(raw, "\n")
	for i := len(lines) - 1; i >= 0; i-- {
		line := strings.TrimSpace(lines[i])
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var info detachLaunchInfo
		if err := json.Unmarshal([]byte(line), &info); err != nil {
			return detachLaunchInfo{}, fmt.Errorf("parse detach launch metadata: %w", err)
		}
		if info.JobID == "" || info.PID <= 0 || info.LogPath == "" || info.MetaPath == "" {
			return detachLaunchInfo{}, errors.New("missing detach launch metadata")
		}
		return info, nil
	}
	return detachLaunchInfo{}, errors.New("missing detach launch metadata")
}

func runDetachExec(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, cmdStr string, fanout int, out io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)
	colorEnabled := isInteractiveWriter(out)
	displayPrefixes := formatPodPrefixes(shortNames, colorEnabled)

	results := make(chan podDetachResult, len(pods))
	sem := make(chan struct{}, fanout)

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			spec := newDetachJobSpec(pod.Name, container, cmdStr)
			command := detachCommand(spec)
			var remoteStdout, remoteStderr bytes.Buffer
			err := connect.RunOnContainer(ctx, client, namespace, pod.Name, container, command, false, nil, &remoteStdout, &remoteStderr)
			if err != nil {
				results <- podDetachResult{pod: pod.Name, err: err}
				return
			}
			info, parseErr := parseDetachLaunchOutput(remoteStdout.String())
			if parseErr != nil {
				if detail := strings.TrimSpace(remoteStderr.String()); detail != "" {
					parseErr = fmt.Errorf("%w: %s", parseErr, detail)
				}
				results <- podDetachResult{pod: pod.Name, err: parseErr}
				return
			}
			results <- podDetachResult{pod: pod.Name, info: info}
		}(pod, shortNames[i])
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	displayMap := make(map[string]string, len(podNames))
	nameMap := make(map[string]string, len(podNames))
	for i, n := range podNames {
		displayMap[n] = displayPrefixes[i]
		nameMap[n] = shortNames[i]
	}

	var failCount int
	for r := range results {
		prefix := displayMap[r.pod]
		if r.err != nil {
			fmt.Fprintf(out, "%s error: %v\n", prefix, r.err)
			failCount++
		} else {
			fmt.Fprintf(out, "%s detached job_id=%s pid=%d log=%s meta=%s\n", prefix, r.info.JobID, r.info.PID, r.info.LogPath, r.info.MetaPath)
		}
	}
	if failCount > 0 {
		return fmt.Errorf("%d of %d pods failed to detach", failCount, len(pods))
	}
	return nil
}

func filterRunningPods(pods []kube.PodSummary) []kube.PodSummary {
	out := make([]kube.PodSummary, 0, len(pods))
	for _, p := range pods {
		if p.Phase == "Running" {
			out = append(out, p)
		}
	}
	return out
}

func filterPodsByRole(pods []kube.PodSummary, role string) []kube.PodSummary {
	out := make([]kube.PodSummary, 0)
	for _, p := range pods {
		if strings.EqualFold(strings.TrimSpace(p.Labels["okdev.io/workload-role"]), role) {
			out = append(out, p)
		}
	}
	return out
}

func filterPodsByLabels(pods []kube.PodSummary, labels []string) []kube.PodSummary {
	selectors := make([][2]string, 0, len(labels))
	for _, l := range labels {
		parts := strings.SplitN(l, "=", 2)
		if len(parts) != 2 {
			continue
		}
		selectors = append(selectors, [2]string{parts[0], parts[1]})
	}
	out := make([]kube.PodSummary, 0)
	for _, p := range pods {
		match := true
		for _, sel := range selectors {
			if p.Labels[sel[0]] != sel[1] {
				match = false
				break
			}
		}
		if match {
			out = append(out, p)
		}
	}
	return out
}

func filterPodsByName(pods []kube.PodSummary, names []string) []kube.PodSummary {
	nameSet := make(map[string]bool, len(names))
	for _, n := range names {
		nameSet[n] = true
	}
	out := make([]kube.PodSummary, 0)
	for _, p := range pods {
		if nameSet[p.Name] {
			out = append(out, p)
		}
	}
	return out
}

func excludePods(pods []kube.PodSummary, exclude []string) []kube.PodSummary {
	excludeSet := make(map[string]bool, len(exclude))
	for _, n := range exclude {
		excludeSet[n] = true
	}
	out := make([]kube.PodSummary, 0, len(pods))
	for _, p := range pods {
		if !excludeSet[p.Name] {
			out = append(out, p)
		}
	}
	return out
}
