package cli

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/shellutil"
	"github.com/spf13/cobra"
)

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
				return runMultiPodExec(cmd, cc, commandArgs, podNames, role, labels, exclude, container, detach, timeout, logDir, noPrefix, fanout)
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

func runMultiPodExec(cmd *cobra.Command, cc *commandContext, commandArgs []string, podNames []string, role string, labels []string, exclude []string, container string, detach bool, timeout time.Duration, logDir string, noPrefix bool, fanout int) error {
	ctx := cmd.Context()
	labelSel := "okdev.io/managed=true,okdev.io/session=" + cc.sessionName
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return fmt.Errorf("list session pods: %w", err)
	}
	pods := filterRunningPods(sessionPods)

	switch {
	case len(podNames) > 0:
		pods = filterPodsByName(pods, podNames)
	case role != "":
		pods = filterPodsByRole(pods, role)
	case len(labels) > 0:
		pods = filterPodsByLabels(pods, labels)
	}

	if len(exclude) > 0 {
		pods = excludePods(pods, exclude)
	}

	if len(pods) == 0 {
		return fmt.Errorf("no running pods match the specified filters in session %q", cc.sessionName)
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

	var writeMu sync.Mutex
	noPrefixOut := &lockedWriter{w: stdout}
	noPrefixErr := &lockedWriter{w: stderr}
	results := make(chan podExecResult, len(pods))
	sem := make(chan struct{}, fanout)

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName string) {
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
				pw := newPrefixedWriter(shortName, stdout, &writeMu)
				pe := newPrefixedWriter(shortName, stderr, &writeMu)
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
		}(pod, shortNames[i])
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
		nameMap := make(map[string]string, len(podNames))
		for i, n := range podNames {
			nameMap[n] = shortNames[i]
		}
		parts := make([]string, 0, len(failures))
		for _, f := range failures {
			short := nameMap[f.pod]
			parts = append(parts, fmt.Sprintf("%s (%v)", short, f.err))
		}
		fmt.Fprintf(stderr, "FAILED: %s\n", strings.Join(parts, ", "))
		return fmt.Errorf("%d of %d pods failed", len(failures), len(pods))
	}
	return nil
}

func detachCommand(cmd string) []string {
	quoted := shellutil.Quote(cmd)
	return []string{"sh", "-c", "nohup sh -c " + quoted + " >/dev/null 2>&1 &"}
}

func runDetachExec(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, cmdStr string, fanout int, out io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)
	command := detachCommand(cmdStr)

	results := make(chan podExecResult, len(pods))
	sem := make(chan struct{}, fanout)

	var wg sync.WaitGroup
	for i, pod := range pods {
		wg.Add(1)
		go func(pod kube.PodSummary, shortName string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			err := connect.RunOnContainer(ctx, client, namespace, pod.Name, container, command, false, nil, io.Discard, io.Discard)
			results <- podExecResult{pod: pod.Name, err: err}
		}(pod, shortNames[i])
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	nameMap := make(map[string]string, len(podNames))
	for i, n := range podNames {
		nameMap[n] = shortNames[i]
	}

	var failCount int
	for r := range results {
		short := nameMap[r.pod]
		if r.err != nil {
			fmt.Fprintf(out, "%s: error: %v\n", short, r.err)
			failCount++
		} else {
			fmt.Fprintf(out, "%s: detached\n", short)
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
