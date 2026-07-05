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

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/fanout"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/shellutil"
	"github.com/spf13/cobra"
	k8sexec "k8s.io/client-go/util/exec"
)

// detachMetadataDir lives on the okdev-runtime emptyDir volume (mounted at
// /var/okdev in both the target container and the okdev-sidecar), so detached
// job logs and metadata survive a container crash (e.g. OOMKill) and remain
// readable through the sidecar. Containers without that mount (a --container
// override) still work: mkdir -p creates the path on the container overlay,
// with the same lifetime as the old /tmp location.
const detachMetadataDir = "/var/okdev/exec"

// legacyDetachMetadataDir is where detached jobs launched by older okdev
// versions keep their logs and metadata; listing scans it for compatibility.
const legacyDetachMetadataDir = "/tmp/okdev-exec"

const detachPIDPlaceholder = "__OKDEV_DETACH_PID__"
const detachPGIDPlaceholder = "__OKDEV_DETACH_PGID__"
const detachExitPlaceholder = "__OKDEV_DETACH_EXIT__"

// detachDirPlaceholder stands in for the metadata directory in job paths and
// JSON templates until the launcher picks the real directory on the pod:
// detachMetadataDir when the runtime volume is writable, otherwise the legacy
// /tmp location (e.g. a user-supplied okdev-runtime volume that a non-root
// container user cannot write to).
const detachDirPlaceholder = "__OKDEV_DETACH_DIR__"

func newExecCmd(opts *Options) *cobra.Command {
	var shell string
	var scriptPath string
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
	var jsonOutput bool
	var gatewayPod string
	var requireAll bool

	cmd := &cobra.Command{
		Use:   "exec [session]",
		Short: "Open shell or run command in session pod(s)",
		Long:  "Open a shell via kubectl exec, or run a command across multiple pods in parallel (pdsh-style).\nFor persistent tmux sessions and port forwarding, use okdev ssh instead.",
		Example: `  # Open an interactive shell
  okdev exec

  # Run a command on all session pods
  okdev exec -- nvidia-smi

  # Upload and run a local script on all session pods
  okdev exec --script ./collect-logs.sh -- --since 10m

  # Run on worker pods only
  okdev exec --workers -- nvidia-smi

  # Run on explicit pod groups
  okdev exec --group master-0,worker-0 --group master-1,worker-1 --parallel -- ./run-bench.sh

  # Run on a specific role
  okdev exec --role worker -- df -h /workspace

  # Detach a background command on worker pods
  okdev exec --role worker --detach -- python train.py

  # Detach a local script on worker pods
  okdev exec --role worker --detach --script ./benchmark.sh -- --steps 100`,
		Aliases:           []string{"connect"},
		Args:              validateExecArgs,
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			sessionArgs, commandArgs := splitExecArgs(cmd, args)
			invocation, err := buildExecInvocation(commandArgs, scriptPath)
			if err != nil {
				return err
			}
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

			hasCommand := len(invocation.Argv) > 0 || invocation.ScriptLocalPath != ""
			multiPod := hasCommand || allPods || workers || len(podNames) > 0 || role != "" || len(labels) > 0 || len(groups) > 0 || detach
			if err := validateExecMode(shell, invocation); err != nil {
				return err
			}
			if err := validateMultiPodFlags(allPods, workers, podNames, role, labels, exclude, groups, hasCommand, detach, sequential, parallel); err != nil {
				return err
			}
			if err := validateExecJSONFlags(jsonOutput, hasCommand, detach, groups, parallel, sequential, logDir); err != nil {
				return err
			}
			if err := validateExecGatewayFlags(jsonOutput, gatewayPod, requireAll); err != nil {
				return err
			}

			if multiPod || detach {
				return runMultiPodExec(cmd, cc, invocation, plan, container, detach, timeout, logDir, noPrefix, fanout, jsonOutput, gatewayPod, requireAll)
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
			connectErr := runConnectWithClient(cc.kube, cc.namespace, target, execCmd, !noTTY)
			if connectErr != nil && isContainerUnavailableExecError(connectErr) {
				if hint := containerUnavailableHint(cmd.Context(), cc.kube, cc.namespace, target.PodName, target.Container); hint != "" {
					return fmt.Errorf("%w; %s", connectErr, hint)
				}
			}
			return connectErr
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to start (default auto-detects bash/sh)")
	cmd.Flags().StringVar(&scriptPath, "script", "", "Upload and run a local script file")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Disable TTY allocation")
	cmd.Flags().BoolVar(&allPods, "all", false, "Target all session pods explicitly")
	cmd.Flags().BoolVar(&workers, "workers", false, "Target worker-role pods")
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	// StringArrayVar, not StringSliceVar: commas are group-member separators
	// that buildExplicitExecPodGroups parses itself; StringSliceVar would
	// CSV-split each --group value into separate single-pod specs.
	cmd.Flags().StringArrayVar(&groups, "group", nil, "Target explicit pod groups by comma-separated pod names (repeatable)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().BoolVar(&detach, "detach", false, "Launch command in background and return")
	cmd.Flags().DurationVar(&timeout, "timeout", 0, "Per-pod command timeout (e.g., 30s, 5m)")
	cmd.Flags().StringVar(&logDir, "log-dir", "", "Write per-pod logs to directory")
	cmd.Flags().BoolVar(&noPrefix, "no-prefix", false, "Suppress pod name prefix in output (auto-enabled when stdout is not a terminal)")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod executions")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Run only on pods that are already running (skip readiness check)")
	cmd.Flags().BoolVar(&sequential, "sequential", false, "Run selected pods or groups one after another")
	cmd.Flags().BoolVar(&parallel, "parallel", false, "Run explicit groups in parallel")
	cmd.Flags().BoolVar(&jsonOutput, "json", false, "Capture each pod's result into a JSON envelope {pod,exit,stdout,stderr,status}; remote exit codes become data, not okdev failures")
	cmd.Flags().StringVar(&gatewayPod, "gateway", "", "Pod to use as the in-cluster fanout gateway (requires --json and interpod SSH)")
	cmd.Flags().BoolVar(&requireAll, "require-all", false, "Exit non-zero unless every targeted pod responded (requires --json)")
	return cmd
}

func validateExecGatewayFlags(jsonOutput bool, gatewayPod string, requireAll bool) error {
	if jsonOutput {
		return nil
	}
	if strings.TrimSpace(gatewayPod) != "" {
		return fmt.Errorf("--gateway requires --json")
	}
	if requireAll {
		return fmt.Errorf("--require-all requires --json")
	}
	return nil
}

type execInvocation struct {
	Argv             []string
	DisplayCommand   string
	ScriptLocalPath  string
	ScriptHasShebang bool
}

type scriptCopyClient interface {
	connect.ExecClient
	CopyToPodInContainer(ctx context.Context, namespace, localPath, podName, container, remotePath string) error
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
	Container  string
	Invocation execInvocation
	Detach     bool
	Timeout    time.Duration
	LogDir     string
	NoPrefix   bool
	Fanout     int
	Order      execGroupOrder
	Stdout     io.Writer
	Stderr     io.Writer
}

func validateExecMode(shell string, invocation execInvocation) error {
	if strings.TrimSpace(shell) != "" && invocation.ScriptLocalPath != "" {
		return fmt.Errorf("--shell cannot be used with --script")
	}
	return nil
}

func validateMultiPodFlags(allPods bool, workers bool, podNames []string, role string, labels []string, exclude []string, groups []string, hasCommand bool, detach bool, sequential bool, parallel bool) error {
	usesSelector := allPods || workers || len(podNames) > 0 || role != "" || len(labels) > 0 || len(exclude) > 0 || len(groups) > 0
	if (usesSelector || detach || sequential || parallel) && !hasCommand {
		return fmt.Errorf("command after -- or --script is required when using --all, --workers, --pod, --role, --label, --exclude, --group, --sequential, --parallel, or --detach")
	}
	if sequential && parallel {
		return fmt.Errorf("--parallel and --sequential are mutually exclusive")
	}
	if parallel && len(groups) == 0 {
		return fmt.Errorf("--parallel requires one or more --group flags")
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

// validateExecJSONFlags enforces the --json contract: it is a capture-and-emit
// mode for a single command run across the selected pods. It is incompatible
// with modes whose output contract differs (--detach emits launch records;
// --group/--parallel/--sequential interleave group headers; --log-dir streams
// per-pod files). The captured stdout/stderr ARE the structured artifact.
func validateExecJSONFlags(jsonOutput, hasCommand, detach bool, groups []string, parallel, sequential bool, logDir string) error {
	if !jsonOutput {
		return nil
	}
	if !hasCommand {
		return fmt.Errorf("--json requires a command after -- or --script")
	}
	switch {
	case detach:
		return fmt.Errorf("--json cannot be used with --detach")
	case len(groups) > 0:
		return fmt.Errorf("--json cannot be used with --group")
	case parallel:
		return fmt.Errorf("--json cannot be used with --parallel")
	case sequential:
		return fmt.Errorf("--json cannot be used with --sequential")
	case strings.TrimSpace(logDir) != "":
		return fmt.Errorf("--json cannot be used with --log-dir")
	}
	return nil
}

// resolveExecNoPrefix decides whether to suppress the per-pod output prefix.
// The `[pod]` prefix is a TTY convenience for disambiguating interleaved
// output; when stdout is piped/redirected (the scripted / agent case) it is
// pure noise callers have to strip before parsing, so it is auto-suppressed.
// An explicit --no-prefix=<value> always wins. The check is keyed on raw
// isatty (isTerminalWriter), not isInteractiveWriter, so NO_COLOR / TERM=dumb
// on a real terminal still keeps the prefix.
func resolveExecNoPrefix(explicitlySet bool, noPrefix bool, out io.Writer) bool {
	if explicitlySet {
		return noPrefix
	}
	return !isTerminalWriter(out)
}

func runMultiPodExec(cmd *cobra.Command, cc *commandContext, invocation execInvocation, plan execTargetPlan, container string, detach bool, timeout time.Duration, logDir string, noPrefix bool, fanout int, jsonOutput bool, gatewayPod string, requireAll bool) error {
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

	if jsonOutput {
		// validateExecJSONFlags guarantees no --group, so there is exactly one
		// selection group here.
		effectiveFanout := fanout
		if effectiveFanout <= 0 {
			effectiveFanout = pdshDefaultFanout
		}
		return runExecJSONRouted(ctx, cc.kube, cc.cfg, cc.namespace, groups[0].Pods, targetContainer, invocation, timeout, effectiveFanout, gatewayPod, requireAll, cmd.OutOrStdout(), cmd.ErrOrStderr())
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
	if plan.sequential && len(plan.groups) == 0 {
		// --sequential without --group serializes pods within the single
		// selection group, keeping the flat log layout and short prefixes.
		effectiveFanout = 1
	}

	order := execGroupOrderSequential
	if plan.parallel {
		order = execGroupOrderParallel
	}

	effectiveNoPrefix := resolveExecNoPrefix(cmd.Flags().Changed("no-prefix"), noPrefix, cmd.OutOrStdout())

	return runExecPodGroups(ctx, cc.kube, cc.namespace, groups, execGroupRunOptions{
		Container:  targetContainer,
		Invocation: invocation,
		Detach:     detach,
		Timeout:    timeout,
		LogDir:     logDir,
		NoPrefix:   effectiveNoPrefix,
		Fanout:     effectiveFanout,
		Order:      order,
		Stdout:     cmd.OutOrStdout(),
		Stderr:     cmd.ErrOrStderr(),
	})
}

func buildExecInvocation(commandArgs []string, scriptPath string) (execInvocation, error) {
	invocation := execInvocation{
		Argv:           append([]string(nil), commandArgs...),
		DisplayCommand: shellQuotedArgs(commandArgs),
	}
	if strings.TrimSpace(scriptPath) == "" {
		return invocation, nil
	}

	info, err := os.Stat(scriptPath)
	if err != nil {
		return execInvocation{}, fmt.Errorf("stat script: %w", err)
	}
	if info.IsDir() {
		return execInvocation{}, fmt.Errorf("script path must be a file: %s", scriptPath)
	}
	hasShebang, err := localFileHasShebang(scriptPath)
	if err != nil {
		return execInvocation{}, err
	}
	invocation.ScriptLocalPath = scriptPath
	invocation.ScriptHasShebang = hasShebang
	invocation.DisplayCommand = shellQuotedArgs(append([]string{scriptPath}, commandArgs...))
	return invocation, nil
}

func localFileHasShebang(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, fmt.Errorf("open script: %w", err)
	}
	defer f.Close()
	var prefix [2]byte
	n, err := f.Read(prefix[:])
	if err != nil && !errors.Is(err, io.EOF) {
		return false, fmt.Errorf("read script: %w", err)
	}
	return n == 2 && string(prefix[:]) == "#!", nil
}

// remoteExecScriptPath returns a per-call destination for an uploaded script.
// Files live directly in /tmp so the upload does not depend on a parent dir
// existing on the target container. The "okdev-exec-script-" prefix is stable
// so leftover files (after SIGKILL or a torn-down stream) are discoverable via
// `ls /tmp/okdev-exec-script-*`.
func remoteExecScriptPath() string {
	return filepath.Join("/tmp", "okdev-exec-script-"+newDetachJobID())
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

	running, err := readyExecPods(sessionName, "", filtered, plan.readyOnly)
	if err != nil {
		return nil, err
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
	groupOfPod := make(map[string]int)
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
			if prev, ok := groupOfPod[pod.Name]; ok {
				return nil, fmt.Errorf("pod %q appears in both group %d and group %d", pod.Name, prev, i+1)
			}
			groupOfPod[pod.Name] = i + 1
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

// readyExecPods enforces the running-vs-targeted readiness check. scope is an
// optional human-readable label (for example "group 2") that is woven into the
// error message when set; callers that don't operate on a named group (the
// shared selectSessionPods helper, exec-jobs, port-forward) should pass "".
func readyExecPods(sessionName string, scope string, targeted []kube.PodSummary, readyOnly bool) ([]kube.PodSummary, error) {
	scopeSuffix := ""
	scopePrefix := ""
	if scope != "" {
		scopeSuffix = " (" + scope + ")"
		scopePrefix = scope + ": "
	}
	if len(targeted) == 0 {
		return nil, fmt.Errorf("no pods match the specified filters in session %q%s", sessionName, scopeSuffix)
	}
	pods := filterRunningPods(targeted)
	if len(pods) == 0 {
		return nil, fmt.Errorf("no running pods in session %q%s (0/%d pods ready)", sessionName, scopeSuffix, len(targeted))
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
		return nil, fmt.Errorf("%s%d/%d pods are not running: %s\nUse --ready-only to run on the %d ready pods",
			scopePrefix, len(notReady), len(targeted), strings.Join(notReady, ", "), len(pods))
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

func runExecPodGroups(ctx context.Context, client scriptCopyClient, namespace string, groups []execPodGroup, opts execGroupRunOptions) error {
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

	stdout, stderr := opts.Stdout, opts.Stderr
	runParallel := opts.Order == execGroupOrderParallel && len(groups) > 1
	if runParallel {
		// Each runMultiExec call only synchronizes its own writers, so
		// concurrent groups need a shared lock on the underlying streams.
		stdout = &lockedWriter{w: stdout}
		stderr = &lockedWriter{w: stderr}
	}
	// One semaphore across all groups so --fanout caps total concurrent pod
	// executions, not per-group.
	sem := make(chan struct{}, opts.Fanout)

	runGroup := func(index int, group execPodGroup) error {
		if len(groups) > 1 {
			fmt.Fprintf(stdout, "==> group %d/%d: %s\n", index+1, len(groups), group.Label)
		}
		if opts.Detach {
			// Detached jobs log inside the pod; --log-dir is not used.
			return runDetachExec(ctx, client, namespace, group.Pods, opts.Container, opts.Invocation, sem, stdout)
		}
		groupLogDir, err := groupExecLogDir(opts.LogDir, index, group.Label, len(groups) > 1)
		if err != nil {
			return err
		}
		if opts.Invocation.ScriptLocalPath != "" {
			return runMultiExecScript(ctx, client, namespace, group.Pods, opts.Container, opts.Invocation, groupLogDir, opts.Timeout, opts.NoPrefix, sem, stdout, stderr)
		}
		return runMultiExec(ctx, client, namespace, group.Pods, opts.Container, opts.Invocation.Argv, groupLogDir, opts.Timeout, opts.NoPrefix, sem, stdout, stderr)
	}

	groupErrs := make([]error, len(groups))
	if runParallel {
		var wg sync.WaitGroup
		for i, group := range groups {
			wg.Add(1)
			go func(index int, group execPodGroup) {
				defer wg.Done()
				groupErrs[index] = runGroup(index, group)
			}(i, group)
		}
		wg.Wait()
	} else {
		for i, group := range groups {
			groupErrs[i] = runGroup(i, group)
		}
	}

	if len(groups) == 1 {
		return groupErrs[0]
	}
	var failed []error
	for i, err := range groupErrs {
		if err != nil {
			failed = append(failed, fmt.Errorf("group %d/%d (%s): %w", i+1, len(groups), groups[i].Label, err))
		}
	}
	if len(failed) > 0 {
		return fmt.Errorf("%d of %d groups failed: %w", len(failed), len(groups), errors.Join(failed...))
	}
	return nil
}

func groupExecLogDir(base string, index int, label string, useGroupDir bool) (string, error) {
	if base == "" {
		return "", nil
	}
	if !useGroupDir {
		return base, nil
	}
	sanitized := sanitizeGroupLabel(label)
	dir := filepath.Join(base, fmt.Sprintf("group-%02d-%s", index+1, sanitized))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("create group log directory: %w", err)
	}
	return dir, nil
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
	// Cap like session naming's sanitize(): labels join full pod names, which
	// can exceed the 255-byte filename limit otherwise.
	if len(label) > 50 {
		label = strings.Trim(label[:50], "-")
	}
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

// execJSONResult is the per-pod envelope emitted by `okdev exec --json`. Exit
// carries the remote command's own status as DATA (so `pgrep` returning 1 for
// "no match" is not conflated with an okdev failure). For failures where no
// remote exit code exists (timeout, transport), Exit is -1 and Error holds the
// reason; the command's exit then never masquerades as a real exit status.
type execJSONResult struct {
	Pod    string `json:"pod"`
	Exit   int    `json:"exit"`
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Error  string `json:"error,omitempty"`
	// Status makes per-pod delivery explicit so callers can tell "responded
	// with empty output" from "never responded": responded | unreachable |
	// timeout | error | missing (see internal/fanout statuses).
	Status string `json:"status,omitempty"`
}

// runExecJSONRouted picks the fanout path for a JSON-mode exec: the gateway
// driver (one apiserver exec, fanout over the pod network) when the session
// supports it, or direct per-pod exec otherwise. Results always print as a
// JSON array; with requireAll, a pod whose command outcome is unknown makes
// the command exit non-zero after the document is emitted.
func runExecJSONRouted(ctx context.Context, client scriptCopyClient, cfg *config.DevEnvironment, namespace string, pods []kube.PodSummary, container string, invocation execInvocation, timeout time.Duration, fanoutN int, gatewayPod string, requireAll bool, out, errOut io.Writer) error {
	if len(pods) == 0 {
		return errors.New("no pods selected")
	}
	route := fanoutRouteFromConfig(cfg, gatewayPod)
	var results []execJSONResult
	if useGatewayFanout(route, pods) {
		gw, err := selectGatewayPod(pods, gatewayPod)
		if err != nil {
			return err
		}
		req, err := buildFanoutRequest(route, pods, invocation, timeout, fanoutN)
		if err != nil {
			return err
		}
		gwResults, authoritative, err := runGatewayFanout(ctx, client, namespace, gw, container, req)
		if err != nil {
			return err
		}
		if authoritative {
			results = gwResults
		} else {
			fmt.Fprintf(errOut, "gateway fanout driver unavailable on %s (older sidecar?); falling back to direct exec\n", gw.Name)
		}
	}
	if results == nil {
		var err error
		results, err = runMultiExecJSONResults(ctx, client, namespace, pods, container, invocation, timeout, fanoutN)
		if err != nil {
			return err
		}
	}
	if err := printExecJSONResults(out, results); err != nil {
		return err
	}
	if requireAll {
		return requireAllError(results)
	}
	return nil
}

// runMultiExecJSON runs the command on every selected pod, capturing each pod's
// stdout/stderr and exit status into an envelope rather than streaming. Output
// is always a JSON array with one envelope per pod in selection order — a single
// resolved pod still emits a length-1 array — so callers never branch on
// object-vs-array by pod count. okdev exits 0 whenever the document is produced —
// remote non-zero exits and per-pod infra errors are reported inside the
// envelope, so agent callers get one deterministic parse path.
func runMultiExecJSON(ctx context.Context, client scriptCopyClient, namespace string, pods []kube.PodSummary, container string, invocation execInvocation, timeout time.Duration, fanout int, out io.Writer) error {
	results, err := runMultiExecJSONResults(ctx, client, namespace, pods, container, invocation, timeout, fanout)
	if err != nil {
		return err
	}
	return printExecJSONResults(out, results)
}

func printExecJSONResults(out io.Writer, results []execJSONResult) error {
	// Always emit a JSON array — one envelope per pod, in selection order —
	// even for a single resolved pod, so callers have one deterministic parse
	// path instead of branching on object-vs-array by pod count.
	data, err := json.MarshalIndent(results, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal exec json: %w", err)
	}
	_, err = fmt.Fprintln(out, string(data))
	return err
}

func runMultiExecJSONResults(ctx context.Context, client scriptCopyClient, namespace string, pods []kube.PodSummary, container string, invocation execInvocation, timeout time.Duration, fanoutN int) ([]execJSONResult, error) {
	if len(pods) == 0 {
		return nil, errors.New("no pods selected")
	}
	if fanoutN <= 0 {
		fanoutN = pdshDefaultFanout
	}
	sem := make(chan struct{}, fanoutN)
	results := make([]execJSONResult, len(pods))

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(i int, pod kube.PodSummary) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			execCtx := ctx
			var cancel context.CancelFunc
			if timeout > 0 {
				execCtx, cancel = context.WithTimeout(ctx, timeout)
				defer cancel()
			}

			res := execJSONResult{Pod: pod.Name}
			command := invocation.Argv
			if invocation.ScriptLocalPath != "" {
				remotePath := remoteExecScriptPath()
				if err := client.CopyToPodInContainer(execCtx, namespace, invocation.ScriptLocalPath, pod.Name, container, remotePath); err != nil {
					res.Exit = -1
					res.Status = fanout.StatusError
					res.Error = "upload script: " + collapseWhitespace(err.Error())
					results[i] = res
					return
				}
				command = scriptExecCommand(remotePath, invocation.ScriptHasShebang, invocation.Argv, true)
			}

			var stdoutBuf, stderrBuf bytes.Buffer
			err := connect.RunOnContainer(execCtx, client, namespace, pod.Name, container, command, false, nil, &stdoutBuf, &stderrBuf)
			res.Stdout = stdoutBuf.String()
			res.Stderr = stderrBuf.String()
			if err == nil {
				res.Exit = 0
				res.Status = fanout.StatusResponded
			} else if kind, code := classifyPodExecFailure(err); kind == "remote-exit" {
				res.Exit = code
				res.Status = fanout.StatusResponded
			} else {
				res.Exit = -1
				if kind == "timeout" {
					res.Status = fanout.StatusTimeout
				} else {
					res.Status = fanout.StatusError
				}
				res.Error = collapseWhitespace(err.Error())
			}
			results[i] = res
		}(i, pod)
	}
	wg.Wait()
	return results, nil
}

func collapseWhitespace(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

func runMultiExec(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, command []string, logDir string, timeout time.Duration, noPrefix bool, sem chan struct{}, stdout, stderr io.Writer) error {
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
			part := formatPodExecFailureSummary(f.pod, container, f.err)
			if isContainerUnavailableExecError(f.err) {
				if hint := containerUnavailableHint(ctx, client, namespace, f.pod, container); hint != "" {
					part += " " + hint
				}
			}
			parts = append(parts, part)
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
	Argv       []string
	Cleanup    []string
	StartedAt  string
	LogPath    string
	MetaPath   string
	ScriptPath string
}

type detachMetadata struct {
	JobID     string `json:"jobId"`
	Pod       string `json:"pod"`
	Container string `json:"container"`
	PID       int    `json:"pid"`
	// PGID is the job's process group id (== PID when the wrapper launched
	// the command via setsid); 0 for jobs from containers without setsid or
	// launched by older okdev versions — group-wide signaling is skipped.
	PGID       int    `json:"pgid"`
	Command    string `json:"command"`
	StartedAt  string `json:"startedAt"`
	StdoutPath string `json:"stdoutPath"`
	StderrPath string `json:"stderrPath"`
	MetaPath   string `json:"metaPath"`
	State      string `json:"state"`
	ExitCode   *int   `json:"exitCode,omitempty"`
	// GroupLive is the count of live (non-zombie) processes still in the
	// job's process group at scan time, verified to belong to this job via
	// OKDEV_JOB_ID in a member's environment. Populated from the scan line
	// prefix, not from the metadata file itself.
	GroupLive int `json:"-"`
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
	wrapper := detachWrapperScript(spec.JobID, spec.Argv, spec.Cleanup, spec.MetaPath, runningTemplate, completionTemplate)
	script := fmt.Sprintf(
		"set -eu\n"+
			// Prefer the shared runtime volume so logs survive a container
			// crash; fall back to the legacy /tmp location when the volume is
			// absent or not writable by the container user (e.g. a
			// user-supplied okdev-runtime volume with root-only permissions).
			"dir=%s\n"+
			"if ! mkdir -p \"$dir\" 2>/dev/null || [ ! -w \"$dir\" ]; then\n"+
			"  dir=%s\n"+
			"  echo \"okdev: detach dir not writable, using $dir\" >&2\n"+
			"  mkdir -p \"$dir\"\n"+
			"fi\n"+
			"log_path=\"$dir\"/%s\n"+
			"meta_path=\"$dir\"/%s\n"+
			"wrapper_path=\"$dir\"/%s\n"+
			"launch_json=%s\n"+
			"sed \"s|%s|$dir|g\" >\"$wrapper_path\" <<'OKDEV_DETACH_WRAPPER'\n%s\nOKDEV_DETACH_WRAPPER\n"+
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
			"printf '%%s\\n' \"$launch_json\" | sed \"s|%s|$dir|g; s/%s/$pid/g\"\n",
		shellutil.Quote(detachMetadataDir),
		shellutil.Quote(legacyDetachMetadataDir),
		shellutil.Quote(filepath.Base(spec.LogPath)),
		shellutil.Quote(filepath.Base(spec.MetaPath)),
		shellutil.Quote(filepath.Base(spec.ScriptPath)),
		shellutil.Quote(launchTemplate),
		detachDirPlaceholder,
		wrapper,
		detachDirPlaceholder,
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
func detachWrapperScript(jobID string, argv []string, cleanupPaths []string, metaPath, runningTemplate, completionTemplate string) string {
	quotedArgs := shellQuotedArgs(argv)
	cleanupLines := ""
	for _, path := range cleanupPaths {
		if strings.TrimSpace(path) == "" {
			continue
		}
		cleanupLines += fmt.Sprintf("rm -f %s 2>/dev/null || true\n", shellutil.Quote(path))
	}
	if cleanupLines == "" {
		// POSIX sh rejects an empty function body, so always emit at least
		// a no-op. Without this the wrapper script aborts before publishing
		// metadata whenever no cleanup paths are supplied (the common case
		// for non-script `okdev exec --detach`).
		cleanupLines = ":\n"
	}
	return fmt.Sprintf(
		"meta_path=%s\n"+
			"meta_tmp=\"${meta_path}.tmp\"\n"+
			"running_json=%s\n"+
			"exit_json=%s\n"+
			"export OKDEV_JOB_ID=%s\n"+
			"set -- %s\n"+
			"cleanup() {\n%s}\n"+
			// setsid puts the user command in its own process group (PGID ==
			// its PID) so `okdev jobs stop` can signal the whole tree —
			// torchrun's children included. The backgrounded child is not a
			// group leader here, so setsid(2) succeeds without forking and
			// $! is the user command's own pid after exec. Without setsid we
			// keep the old single-process behavior and record pgid=0 so stop
			// falls back to leader-only signaling.
			"if command -v setsid >/dev/null 2>&1; then\n"+
			"  setsid \"$@\" &\n"+
			"  user_pid=$!\n"+
			"  user_pgid=$user_pid\n"+
			"else\n"+
			"  echo 'okdev: setsid unavailable; job has no process group (stop signals the leader only)' >&2\n"+
			"  (exec \"$@\") &\n"+
			"  user_pid=$!\n"+
			"  user_pgid=0\n"+
			"fi\n"+
			"if [ -z \"$user_pid\" ] || [ \"$user_pid\" -le 0 ]; then\n"+
			"  exit 1\n"+
			"fi\n"+
			"printf '%%s\\n' \"$running_json\" | sed \"s/%s/$user_pid/g; s/%s/$user_pgid/g\" >\"$meta_tmp\" || exit 1\n"+
			"mv \"$meta_tmp\" \"$meta_path\" || exit 1\n"+
			"trap 'rc=$?; printf \"%%s\\n\" \"$exit_json\" | sed \"s/%s/$user_pid/g; s/%s/$user_pgid/g; s/%s/$rc/g\" >\"$meta_tmp\" 2>/dev/null && mv \"$meta_tmp\" \"$meta_path\" 2>/dev/null; cleanup; exit $rc' EXIT\n"+
			"wait \"$user_pid\"\n",
		shellutil.Quote(metaPath),
		shellutil.Quote(runningTemplate),
		shellutil.Quote(completionTemplate),
		shellutil.Quote(jobID),
		quotedArgs,
		cleanupLines,
		detachPIDPlaceholder,
		detachPGIDPlaceholder,
		detachPIDPlaceholder,
		detachPGIDPlaceholder,
		detachExitPlaceholder,
	)
}

func mustDetachJSONTemplate(v any) string {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	// Replace "pgid" before "pid": the two keys share a suffix and the pgid
	// replacement must not consume the pid occurrence.
	out := strings.Replace(string(data), `"pgid":0`, `"pgid":`+detachPGIDPlaceholder, 1)
	out = strings.Replace(out, `"pid":0`, `"pid":`+detachPIDPlaceholder, 1)
	return out
}

func mustDetachJSONTemplateWithExit(v detachMetadata) string {
	out := mustDetachJSONTemplate(v)
	out = strings.Replace(out, `"exitCode":0`, `"exitCode":`+detachExitPlaceholder, 1)
	return out
}

func newDetachJobSpec(jobID, pod, container, cmd string, argv []string, cleanupPaths []string) detachJobSpec {
	if strings.TrimSpace(jobID) == "" {
		jobID = newDetachJobID()
	}
	startedAt := time.Now().UTC().Format(time.RFC3339)
	// Paths carry detachDirPlaceholder; the launcher script resolves it to
	// the real directory on the pod (runtime volume, or legacy /tmp when
	// that volume is not writable) and seds it into the JSON templates.
	logPath := detachDirPlaceholder + "/" + jobID + ".log"
	metaPath := detachDirPlaceholder + "/" + jobID + ".json"
	scriptPath := detachDirPlaceholder + "/" + jobID + ".sh"
	return detachJobSpec{
		JobID:      jobID,
		Pod:        pod,
		Container:  container,
		Command:    cmd,
		Argv:       append([]string(nil), argv...),
		Cleanup:    append([]string(nil), cleanupPaths...),
		StartedAt:  startedAt,
		LogPath:    logPath,
		MetaPath:   metaPath,
		ScriptPath: scriptPath,
	}
}

func shellQuotedArgs(args []string) string {
	parts := make([]string, 0, len(args))
	for _, arg := range args {
		parts = append(parts, shellutil.Quote(arg))
	}
	return strings.Join(parts, " ")
}

func scriptExecCommand(remotePath string, hasShebang bool, args []string, cleanup bool) []string {
	runExpr := "\"$script_path\" \"$@\""
	if !hasShebang {
		runExpr = "sh \"$script_path\" \"$@\""
	}
	// When cleanup is requested we install it as a trap so the uploaded
	// script is removed even if the remote shell is signalled (SIGTERM/INT
	// from `kubectl exec` teardown or a `--timeout` cancellation) before the
	// script returns. We preserve $? across the trap so the wrapper exits
	// with the user script's status, matching the detach wrapper's pattern.
	trapLine := ""
	if cleanup {
		trapLine = "trap 'rc=$?; rm -f \"$script_path\" 2>/dev/null; exit $rc' EXIT INT TERM\n"
	}
	script := fmt.Sprintf(
		"script_path=$1\n"+
			"shift\n"+
			"%s"+
			"chmod 700 \"$script_path\" || exit 1\n"+
			"%s\n",
		trapLine,
		runExpr,
	)
	return append([]string{"sh", "-lc", script, "okdev-script", remotePath}, args...)
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

func runDetachExec(ctx context.Context, client scriptCopyClient, namespace string, pods []kube.PodSummary, container string, invocation execInvocation, sem chan struct{}, out io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)
	colorEnabled := isInteractiveWriter(out)
	displayPrefixes := formatPodPrefixes(shortNames, colorEnabled)

	results := make(chan podDetachResult, len(pods))
	jobID := newDetachJobID()

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			argv := append([]string(nil), invocation.Argv...)
			cleanup := []string(nil)
			if invocation.ScriptLocalPath != "" {
				remotePath := remoteExecScriptPath()
				if err := client.CopyToPodInContainer(ctx, namespace, invocation.ScriptLocalPath, pod.Name, container, remotePath); err != nil {
					results <- podDetachResult{pod: pod.Name, err: fmt.Errorf("upload script: %w", err)}
					return
				}
				argv = scriptExecCommand(remotePath, invocation.ScriptHasShebang, invocation.Argv, false)
				cleanup = []string{remotePath}
			}
			spec := newDetachJobSpec(jobID, pod.Name, container, invocation.DisplayCommand, argv, cleanup)
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

func runMultiExecScript(ctx context.Context, client scriptCopyClient, namespace string, pods []kube.PodSummary, container string, invocation execInvocation, logDir string, timeout time.Duration, noPrefix bool, sem chan struct{}, stdout, stderr io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)
	colorEnabled := isInteractiveWriter(stdout)
	displayPrefixes := formatPodPrefixes(shortNames, colorEnabled)

	results := make(chan podExecResult, len(pods))
	var wg sync.WaitGroup
	var writeMu sync.Mutex

	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName string, displayPrefix string) {
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
				podStdout = stdout
				podStderr = stderr
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

			remotePath := remoteExecScriptPath()
			if err := client.CopyToPodInContainer(execCtx, namespace, invocation.ScriptLocalPath, pod.Name, container, remotePath); err != nil {
				results <- podExecResult{pod: pod.Name, err: fmt.Errorf("upload script: %w", err)}
				return
			}
			command := scriptExecCommand(remotePath, invocation.ScriptHasShebang, invocation.Argv, true)
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
			part := formatPodExecFailureSummary(f.pod, container, f.err)
			if isContainerUnavailableExecError(f.err) {
				if hint := containerUnavailableHint(ctx, client, namespace, f.pod, container); hint != "" {
					part += " " + hint
				}
			}
			parts = append(parts, part)
		}
		fmt.Fprintf(stderr, "\nFAILED:\n%s\n", strings.Join(parts, "\n"))
		return fmt.Errorf("%d of %d pods failed", len(failures), len(pods))
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
