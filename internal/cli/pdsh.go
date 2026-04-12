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
	var cmdStr string
	var noTTY bool

	cmd := &cobra.Command{
		Use:   "exec [session]",
		Short: "Open shell or run command in session pod",
		Long:  "Open a shell via kubectl exec. For persistent tmux sessions and port forwarding, use okdev ssh instead.",
		Example: `  # Open an interactive shell
  okdev exec

  # Run a one-off command
  okdev exec --cmd "cat /etc/os-release"

  # Use a specific shell
  okdev exec --shell /bin/zsh`,
		Aliases:           []string{"connect"},
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: sessionCompletionFunc(opts),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			stopMaintenance := startSessionMaintenanceWithClient(cc.kube, cc.namespace, cc.sessionName, cmd.OutOrStdout(), true)
			defer stopMaintenance()

			execCmd := []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			if shell != "" {
				execCmd = []string{shell}
			}
			if cmdStr != "" {
				execCmd = []string{"sh", "-lc", cmdStr}
			}
			if len(args) > 0 {
				execCmd = args
			}
			if len(execCmd) == 1 && strings.TrimSpace(execCmd[0]) == "" {
				execCmd = []string{"sh", "-lc", "command -v bash >/dev/null 2>&1 && exec bash || exec sh"}
			}
			return runConnectWithClient(cc.kube, cc.namespace, target, execCmd, !noTTY)
		},
	}
	cmd.Flags().StringVar(&shell, "shell", "", "Shell to start (default auto-detects bash/sh)")
	cmd.Flags().StringVar(&cmdStr, "cmd", "", "Command string to execute")
	cmd.Flags().BoolVar(&noTTY, "no-tty", false, "Disable TTY allocation")
	return cmd
}

type podExecResult struct {
	pod string
	err error
}

func runMultiExec(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, command []string, logDir string, timeout time.Duration, noPrefix bool, stdout, stderr io.Writer) error {
	podNames := make([]string, len(pods))
	for i, p := range pods {
		podNames[i] = p.Name
	}
	shortNames := shortPodNames(podNames)

	var writeMu sync.Mutex
	results := make(chan podExecResult, len(pods))
	sem := make(chan struct{}, pdshDefaultFanout)

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
				podStdout = stdout
				podStderr = stderr
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
