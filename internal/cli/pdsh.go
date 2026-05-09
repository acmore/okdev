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
	var allPods bool
	var workers bool
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var groups []string
	var container string
	var detach bool
	var timeout time.Duration
	var logDir string
	var noPrefix bool
	var fanout int
	var readyOnly bool
	var sequential bool
	var parallel bool

	cmd := &cobra.Command{
		Use:   "exec [session]",
		Short: "Open shell or run command in session pod(s)",
		Long:  "Open a shell via kubectl exec, or run a command across multiple pods in parallel (pdsh-style).\nFor persistent tmux sessions and port forwarding, use okdev ssh instead.",
		Example: `  # Open an interactive shell
  okdev exec

  # Run a command on all session pods
  okdev exec -- nvidia-smi

  # Run on worker pods only
  okdev exec --workers -- nvidia-smi

  # Run on explicit pod groups
  okdev exec --group master-0,worker-0 --group master-1,worker-1 --parallel -- ./run-bench.sh

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

			plan := execTargetPlan{
				allPods:    allPods,
				workers:    workers,
				podNames:   podNames,
				role:       role,
				labels:     labels,
				exclude:    exclude,
				groups:     groups,
				readyOnly:  readyOnly,
				sequential: sequential,
				parallel:   parallel,
			}

			multiPod := len(commandArgs) > 0 || allPods || workers || len(podNames) > 0 || role != "" || len(labels) > 0 || len(groups) > 0 || detach
			if err := validateMultiPodFlags(allPods, workers, podNames, role, labels, exclude, groups, len(commandArgs) > 0, detach, sequential, parallel); err != nil {
				return err
			}

			if multiPod || detach {
				return runMultiPodExec(cmd, cc, commandArgs, plan, container, detach, timeout, logDir, noPrefix, fanout)
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
	cmd.Flags().BoolVar(&allPods, "all", false, "Target all session pods explicitly")
	cmd.Flags().BoolVar(&workers, "workers", false, "Target worker-role pods")
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().StringSliceVar(&groups, "group", nil, "Target explicit pod groups by comma-separated pod names (repeatable)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().BoolVar(&detach, "detach", false, "Launch command in background and return")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Per-pod command timeout (e.g., 30s, 5m)")
	cmd.Flags().StringVar(&logDir, "log-dir", "", "Write per-pod logs to directory")
	cmd.Flags().BoolVar(&noPrefix, "no-prefix", false, "Suppress pod name prefix in output")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod executions")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Run only on pods that are already running (skip readiness check)")
	cmd.Flags().BoolVar(&sequential, "sequential", false, "Run selected pods or groups one after another")
	cmd.Flags().BoolVar(&parallel, "parallel", false, "Run explicit groups in parallel")
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

type execTargetPlan struct {
	allPods    bool
	workers    bool
	podNames   []string
	role       string
	labels     []string
	exclude    []string
	groups     []string
	readyOnly  bool
	sequential bool
	parallel   bool
}

type execPodGroup struct {
	Label string
	Pods  []kube.PodSummary
}

type execGroupOrder int

const (
	execGroupOrderSequential execGroupOrder = iota
	execGroupOrderParallel
)

type execGroupRunOptions struct {
	Container      string
	Command        []string
	DisplayCommand string
	Detach         bool
	Timeout        time.Duration
	LogDir         string
	NoPrefix       bool
	Fanout         int
	Order          execGroupOrder
	Stdout         io.Writer
	Stderr         io.Writer
}

func validateMultiPodFlags(allPods bool, workers bool, podNames []string, role string, labels []string, exclude []string, groups []string, hasCommand bool, detach bool, sequential bool, parallel bool) error {
	usesSelector := allPods || workers || len(podNames) > 0 || role != "" || len(labels) > 0 || len(exclude) > 0 || len(groups) > 0
	if (usesSelector || detach) && !hasCommand {
		return fmt.Errorf("command after -- is required when using --pod, --role, --label, --exclude, or --detach")
	}
	if sequential && parallel {
		return fmt.Errorf("--parallel and --sequential are mutually exclusive")
	}
	if workers && role != "" {
		return fmt.Errorf("--workers cannot be used with --role")
	}
	if len(exclude) > 0 && len(podNames) > 0 {
		return fmt.Errorf("--exclude cannot be used with --pod")
	}
	if len(groups) > 0 && (allPods || workers || len(podNames) > 0 || role != "" || len(labels) > 0 || len(exclude) > 0) {
		return fmt.Errorf("--group cannot be used with --pod, --role, --label, --exclude, --all, or --workers")
	}
	if !hasCommand && !detach {
		return nil
	}
	exclusiveCount := 0
	if allPods {
		exclusiveCount++
	}
	if workers {
		exclusiveCount++
	}
	if len(podNames) > 0 {
		exclusiveCount++
	}
	if role != "" {
		exclusiveCount++
	}
	if len(labels) > 0 {
		exclusiveCount++
	}
	if len(groups) > 0 {
		exclusiveCount++
	}
	if exclusiveCount > 1 {
		return fmt.Errorf("--all, --workers, --pod, --role, --label, and --group are mutually exclusive")
	}
	return nil
}

func runMultiPodExec(cmd *cobra.Command, cc *commandContext, commandArgs []string, plan execTargetPlan, container string, detach bool, timeout time.Duration, logDir string, noPrefix bool, fanout int) error {
	ctx := cmd.Context()
	labelSel := selectorForSessionRun(cc.sessionName)
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return fmt.Errorf("list session pods: %w", err)
	}
	groups, err := buildExecPodGroups(cc.sessionName, sessionPods, plan)
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

	order := execGroupOrderParallel
	if plan.sequential {
		order = execGroupOrderSequential
	}
	return runExecPodGroups(ctx, cc.kube, cc.namespace, groups, execGroupRunOptions{
		Container:      targetContainer,
		Command:        commandArgs,
		DisplayCommand: commandArgsShellString(commandArgs),
		Detach:         detach,
		Timeout:        timeout,
		LogDir:         logDir,
		NoPrefix:       noPrefix,
		Fanout:         effectiveFanout,
		Order:          order,
		Stdout:         cmd.OutOrStdout(),
		Stderr:         cmd.ErrOrStderr(),
	})
}

func commandArgsShellString(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellutil.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func buildExecPodGroups(sessionName string, sessionPods []kube.PodSummary, plan execTargetPlan) ([]execPodGroup, error) {
	if len(plan.groups) > 0 {
		return buildExplicitExecPodGroups(sessionName, sessionPods, plan.groups, plan.readyOnly)
	}

	filtered := sessionPods
	switch {
	case len(plan.podNames) > 0:
		filtered = filterPodsByName(filtered, plan.podNames)
	case plan.workers:
		filtered = filterPodsByRole(filtered, "worker")
	case plan.role != "":
		filtered = filterPodsByRole(filtered, plan.role)
	case len(plan.labels) > 0:
		filtered = filterPodsByLabels(filtered, plan.labels)
	case plan.allPods:
		// keep all
	default:
		// command with no selector defaults to all pods
	}
	if len(plan.exclude) > 0 {
		filtered = excludePods(filtered, plan.exclude)
	}

	running, err := readyExecPods(sessionName, "selection", filtered, plan.readyOnly)
	if err != nil {
		return nil, err
	}
	if plan.sequential {
		groups := make([]execPodGroup, 0, len(running))
		shorts := shortPodNames(podSummaryNames(running))
		for i, pod := range running {
			groups = append(groups, execPodGroup{
				Label: shorts[i],
				Pods:  []kube.PodSummary{pod},
			})
		}
		return groups, nil
	}
	return []execPodGroup{{
		Label: "all",
		Pods:  running,
	}}, nil
}

func buildExplicitExecPodGroups(sessionName string, sessionPods []kube.PodSummary, specs []string, readyOnly bool) ([]execPodGroup, error) {
	aliasToPod := make(map[string]kube.PodSummary)
	names := podSummaryNames(sessionPods)
	shorts := shortPodNames(names)
	for i, pod := range sessionPods {
		aliasToPod[pod.Name] = pod
		aliasToPod[shorts[i]] = pod
	}

	groups := make([]execPodGroup, 0, len(specs))
	for i, spec := range specs {
		parts := splitAndTrimCSV([]string{spec})
		if len(parts) == 0 {
			return nil, fmt.Errorf("group %d is empty", i+1)
		}
		selected := make([]kube.PodSummary, 0, len(parts))
		seen := make(map[string]bool)
		for _, part := range parts {
			pod, ok := aliasToPod[part]
			if !ok {
				return nil, fmt.Errorf("group %d references unknown pod %q", i+1, part)
			}
			if seen[pod.Name] {
				continue
			}
			seen[pod.Name] = true
			selected = append(selected, pod)
		}
		running, err := readyExecPods(sessionName, fmt.Sprintf("group %d", i+1), selected, readyOnly)
		if err != nil {
			return nil, err
		}
		groups = append(groups, execPodGroup{
			Label: strings.Join(parts, ","),
			Pods:  running,
		})
	}
	return groups, nil
}

func readyExecPods(sessionName string, scope string, targeted []kube.PodSummary, readyOnly bool) ([]kube.PodSummary, error) {
	if len(targeted) == 0 {
		return nil, fmt.Errorf("no pods match the specified filters in session %q", sessionName)
	}
	pods := filterRunningPods(targeted)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no running pods in session %q (0/%d pods ready)", sessionName, len(targeted))
	}
	if len(pods) < len(targeted) && !readyOnly {
		notReady := make([]string, 0, len(targeted)-len(pods))
		runningSet := make(map[string]bool, len(pods))
		for _, p := range pods {
			runningSet[p.Name] = true
		}
		for _, p := range targeted {
			if !runningSet[p.Name] {
				notReady = append(notReady, fmt.Sprintf("%s (%s)", p.Name, p.Phase))
			}
		}
		return nil, fmt.Errorf("%s: %d/%d pods are not running: %s\nUse --ready-only to run on the %d ready pods",
			scope, len(notReady), len(targeted), strings.Join(notReady, ", "), len(pods))
	}
	return pods, nil
}

func podSummaryNames(pods []kube.PodSummary) []string {
	out := make([]string, len(pods))
	for i, pod := range pods {
		out[i] = pod.Name
	}
	return out
}

func splitAndTrimCSV(items []string) []string {
	out := make([]string, 0)
	for _, item := range items {
		for _, part := range strings.Split(item, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				out = append(out, part)
			}
		}
	}
	return out
}

func runExecPodGroups(ctx context.Context, client connect.ExecClient, namespace string, groups []execPodGroup, opts execGroupRunOptions) error {
	if len(groups) == 0 {
		return errors.New("no exec pod groups selected")
	}
	if opts.Fanout <= 0 {
		opts.Fanout = pdshDefaultFanout
	}
	if opts.Stdout == nil {
		opts.Stdout = io.Discard
	}
	if opts.Stderr == nil {
		opts.Stderr = io.Discard
	}

	runGroup := func(index int, group execPodGroup) error {
		if len(groups) > 1 {
			fmt.Fprintf(opts.Stdout, "==> group %d/%d: %s\n", index+1, len(groups), group.Label)
		}
		groupLogDir := groupExecLogDir(opts.LogDir, index, group.Label, len(groups) > 1)
		if opts.Detach {
			return runDetachExec(ctx, client, namespace, group.Pods, opts.Container, opts.DisplayCommand, opts.Fanout, opts.Stdout)
		}
		return runMultiExec(ctx, client, namespace, group.Pods, opts.Container, opts.Command, groupLogDir, opts.Timeout, opts.NoPrefix, opts.Fanout, opts.Stdout, opts.Stderr)
	}

	if opts.Order == execGroupOrderSequential || len(groups) == 1 {
		failures := 0
		for i, group := range groups {
			if err := runGroup(i, group); err != nil {
				failures++
			}
		}
		if failures > 0 {
			return fmt.Errorf("%d of %d groups failed", failures, len(groups))
		}
		return nil
	}

	var wg sync.WaitGroup
	var headerMu sync.Mutex
	results := make(chan error, len(groups))
	for i, group := range groups {
		wg.Add(1)
		go func(index int, group execPodGroup) {
			defer wg.Done()
			if len(groups) > 1 {
				headerMu.Lock()
				fmt.Fprintf(opts.Stdout, "==> group %d/%d: %s\n", index+1, len(groups), group.Label)
				headerMu.Unlock()
			}
			groupLogDir := groupExecLogDir(opts.LogDir, index, group.Label, len(groups) > 1)
			var err error
			if opts.Detach {
				err = runDetachExec(ctx, client, namespace, group.Pods, opts.Container, opts.DisplayCommand, opts.Fanout, opts.Stdout)
			} else {
				err = runMultiExec(ctx, client, namespace, group.Pods, opts.Container, opts.Command, groupLogDir, opts.Timeout, opts.NoPrefix, opts.Fanout, opts.Stdout, opts.Stderr)
			}
			results <- err
		}(i, group)
	}
	wg.Wait()
	close(results)

	failures := 0
	for err := range results {
		if err != nil {
			failures++
		}
	}
	if failures > 0 {
		return fmt.Errorf("%d of %d groups failed", failures, len(groups))
	}
	return nil
}

func groupExecLogDir(base string, index int, label string, useGroupDir bool) string {
	if base == "" {
		return ""
	}
	if !useGroupDir {
		return base
	}
	sanitized := sanitizeGroupLabel(label)
	dir := filepath.Join(base, fmt.Sprintf("group-%02d-%s", index+1, sanitized))
	_ = os.MkdirAll(dir, 0o755)
	return dir
}

func sanitizeGroupLabel(label string) string {
	label = strings.ToLower(label)
	label = strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			return r
		case r >= '0' && r <= '9':
			return r
		default:
			return '-'
		}
	}, label)
	label = strings.Trim(label, "-")
	if label == "" {
		return "pods"
	}
	return label
}

// selectSessionPods preserves the existing shared selection helper used by
// exec-jobs and port-forward. It intentionally exposes only the pre-existing
// selector surface; grouped orchestration stays local to exec.
func selectSessionPods(ctx context.Context, cc *commandContext, podNames []string, role string, labels []string, exclude []string, readyOnly bool) ([]kube.PodSummary, error) {
	labelSel := selectorForSessionRun(cc.sessionName)
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return nil, fmt.Errorf("list session pods: %w", err)
	}
	groups, err := buildExecPodGroups(cc.sessionName, sessionPods, execTargetPlan{
		podNames:  podNames,
		role:      role,
		labels:    labels,
		exclude:   exclude,
		readyOnly: readyOnly,
	})
	if err != nil {
		return nil, err
	}
	return groups[0].Pods, nil
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
