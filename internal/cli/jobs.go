package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
)

var errDetachJobNotFound = errors.New("detached job not found")

var (
	detachJobPollEvery = detachJobPollInterval
	detachJobStopGrace = detachJobStopGracePeriod
)

type detachJobClient interface {
	connect.ExecClient
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	StreamShInContainer(context.Context, string, string, string, string, io.Writer, io.Writer) error
}

func newJobsCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "jobs",
		Short: "Manage detached exec jobs",
	}
	cmd.AddCommand(newJobsListCmd(opts))
	cmd.AddCommand(newJobsLogsCmd(opts))
	cmd.AddCommand(newJobsStopCmd(opts))
	cmd.AddCommand(newJobsWaitCmd(opts))
	return cmd
}

func newJobsListCmd(opts *Options) *cobra.Command {
	return newJobsListCmdWithUse(opts, "list [session]", "List detached jobs")
}

func newExecJobsCmd(opts *Options) *cobra.Command {
	return newJobsListCmdWithUse(opts, "exec-jobs [session]", "List detached exec jobs")
}

func newJobsListCmdWithUse(opts *Options, use string, short string) *cobra.Command {
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var container string
	var jobID string
	var fanout int
	var readyOnly bool

	cmd := &cobra.Command{
		Use:               use,
		Short:             short,
		Args:              validateExecArgs,
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

			pods, err := selectSessionPods(cmd.Context(), cc, podNames, role, labels, exclude, readyOnly)
			if err != nil {
				return err
			}
			targetContainer := container
			if targetContainer == "" {
				targetContainer = resolveTargetContainer(cc.cfg)
			}
			effectiveFanout := fanout
			if effectiveFanout <= 0 {
				effectiveFanout = pdshDefaultFanout
			}
			return runExecJobs(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, jobID, effectiveFanout, cmd.OutOrStdout(), opts.Output == "json", fanoutRouteFromConfig(cc.cfg, ""))
		},
	}
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().StringVar(&jobID, "job-id", "", "Filter to a specific detached exec job id")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Query only pods that are already running (skip readiness check)")
	return cmd
}

func newJobsLogsCmd(opts *Options) *cobra.Command {
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var follow bool
	var container string
	var fanout int
	cmd := &cobra.Command{
		Use:   "logs <job-id> [session]",
		Short: "Show detached job logs",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || len(args) > 2 {
				return errors.New("requires <job-id> and optional [session]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args[1:])
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			pods, err := selectSessionPods(cmd.Context(), cc, podNames, role, labels, exclude, true)
			if err != nil {
				return err
			}
			targetContainer := strings.TrimSpace(container)
			if targetContainer == "" {
				targetContainer = resolveTargetContainer(cc.cfg)
			}
			return runJobsLogs(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, strings.TrimSpace(args[0]), fanoutOrDefault(fanout), follow, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow logs until all pods in the job finish")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	return cmd
}

func newJobsStopCmd(opts *Options) *cobra.Command {
	var container string
	var fanout int
	cmd := &cobra.Command{
		Use:   "stop <job-id> [session]",
		Short: "Stop a detached job",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || len(args) > 2 {
				return errors.New("requires <job-id> and optional [session]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args[1:])
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			pods, err := selectSessionPods(cmd.Context(), cc, nil, "", nil, nil, true)
			if err != nil {
				return err
			}
			targetContainer := strings.TrimSpace(container)
			if targetContainer == "" {
				targetContainer = resolveTargetContainer(cc.cfg)
			}
			return runJobsStop(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, strings.TrimSpace(args[0]), fanoutOrDefault(fanout), cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	return cmd
}

func newJobsWaitCmd(opts *Options) *cobra.Command {
	var container string
	var fanout int
	cmd := &cobra.Command{
		Use:   "wait <job-id> [session]",
		Short: "Wait for a detached job to finish",
		Args: func(cmd *cobra.Command, args []string) error {
			if len(args) == 0 || len(args) > 2 {
				return errors.New("requires <job-id> and optional [session]")
			}
			return nil
		},
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args[1:])
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(cc.opts, cc.kube, cc.namespace, cc.sessionName); err != nil {
				return err
			}
			pods, err := selectSessionPods(cmd.Context(), cc, nil, "", nil, nil, true)
			if err != nil {
				return err
			}
			targetContainer := strings.TrimSpace(container)
			if targetContainer == "" {
				targetContainer = resolveTargetContainer(cc.cfg)
			}
			return runJobsWait(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, strings.TrimSpace(args[0]), fanoutOrDefault(fanout))
		},
	}
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	return cmd
}

func fanoutOrDefault(v int) int {
	if v <= 0 {
		return pdshDefaultFanout
	}
	return v
}

func runJobsLogs(ctx context.Context, client detachJobClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int, follow bool, out io.Writer) error {
	job, podErrors, err := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
	if err != nil {
		printExecJobsErrors(out, podErrors)
		return err
	}
	shortNames := shortPodNames(jobPodNames(job))
	prefixes := formatPodPrefixes(shortNames, false)
	prefixByPod := make(map[string]string, len(job.PodStates))
	for i, row := range job.PodStates {
		prefixByPod[row.Pod] = prefixes[i]
	}

	streamCtx := ctx
	var cancel context.CancelFunc
	if follow {
		streamCtx, cancel = context.WithCancel(ctx)
		defer cancel()
	}

	var (
		wg        sync.WaitGroup
		writeMu   sync.Mutex
		errMu     sync.Mutex
		logErrors []execJobsPodError
	)
	recordErr := func(pod string, err error) {
		if err == nil {
			return
		}
		errMu.Lock()
		defer errMu.Unlock()
		logErrors = append(logErrors, execJobsPodError{Pod: pod, Error: err.Error()})
	}

	for _, row := range job.PodStates {
		row := row
		if strings.TrimSpace(row.LogPath) == "" {
			recordErr(row.Pod, errors.New("missing detached job log path"))
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := streamDetachJobLog(streamCtx, client, namespace, row, prefixByPod[row.Pod], follow, out, &writeMu)
			if follow && streamCtx.Err() != nil && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
				return
			}
			recordErr(row.Pod, err)
		}()
	}

	if !follow {
		wg.Wait()
		allErrs := mergePodErrors(podErrorMap(podErrors), logErrors)
		printExecJobsErrors(out, allErrs)
		if len(allErrs) > 0 {
			return fmt.Errorf("failed to stream detached job %q logs on %d pod(s)", jobID, len(allErrs))
		}
		return nil
	}

	// Accumulate per-pod query errors across the follow loop (dedup by pod,
	// latest message wins) so initial failures and later poll failures both
	// surface in the final FAILED footer.
	followPodErrors := podErrorMap(podErrors)
	ticker := time.NewTicker(detachJobPollEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			cancel()
			wg.Wait()
			return ctx.Err()
		case <-ticker.C:
			current, errs, resolveErr := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
			for _, e := range errs {
				followPodErrors[e.Pod] = e.Error
			}
			if resolveErr != nil {
				cancel()
				wg.Wait()
				printExecJobsErrors(out, mergePodErrors(followPodErrors, logErrors))
				return resolveErr
			}
			if len(errs) > 0 {
				cancel()
				wg.Wait()
				allErrs := mergePodErrors(followPodErrors, logErrors)
				printExecJobsErrors(out, allErrs)
				return fmt.Errorf("failed to follow detached job %q logs on %d pod(s)", jobID, len(allErrs))
			}
			if allLogicalJobTerminal(current) {
				cancel()
				wg.Wait()
				allErrs := mergePodErrors(followPodErrors, logErrors)
				printExecJobsErrors(out, allErrs)
				if len(allErrs) > 0 {
					return fmt.Errorf("failed to follow detached job %q logs on %d pod(s)", jobID, len(allErrs))
				}
				return nil
			}
		}
	}
}

func podErrorMap(errs []execJobsPodError) map[string]string {
	m := make(map[string]string, len(errs))
	for _, e := range errs {
		m[e.Pod] = e.Error
	}
	return m
}

func runJobsWait(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int) error {
	ticker := time.NewTicker(detachJobPollEvery)
	defer ticker.Stop()
	for {
		job, podErrors, err := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
		if err != nil {
			return err
		}
		if len(podErrors) > 0 {
			return fmt.Errorf("failed to query detached job %q on %d pod(s)", jobID, len(podErrors))
		}
		if allLogicalJobTerminal(job) {
			if logicalJobFailed(job) {
				return fmt.Errorf("detached job %q finished with state %s", jobID, job.SummaryState)
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

func runJobsStop(ctx context.Context, client detachJobClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int, out io.Writer) error {
	job, initialPodErrors, err := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
	if err != nil {
		printExecJobsErrors(out, initialPodErrors)
		return err
	}

	shortNames := shortPodNames(jobPodNames(job))
	prefixes := formatPodPrefixes(shortNames, false)
	prefixByPod := make(map[string]string, len(job.PodStates))
	for i, row := range job.PodStates {
		prefixByPod[row.Pod] = prefixes[i]
	}

	// Accumulate per-pod query errors across both the SIGTERM wait phase and
	// the final verification poll (dedup by pod) so transient errors don't
	// silently shadow earlier signal failures and vice versa.
	aggregated := podErrorMap(initialPodErrors)

	signalErrors := make([]execJobsPodError, 0)
	for _, row := range job.PodStates {
		if row.State != "running" {
			continue
		}
		result, signalErr := signalDetachJob(ctx, client, namespace, row, "TERM")
		if signalErr != nil {
			signalErrors = append(signalErrors, execJobsPodError{Pod: row.Pod, Error: signalErr.Error()})
			fmt.Fprintf(out, "%s error: %v\n", prefixByPod[row.Pod], signalErr)
			continue
		}
		fmt.Fprintf(out, "%s sent SIGTERM pid=%d (%s)\n", prefixByPod[row.Pod], row.PID, result)
	}
	for _, e := range signalErrors {
		aggregated[e.Pod] = e.Error
	}

	deadline := time.NewTimer(detachJobStopGrace)
	defer deadline.Stop()
	ticker := time.NewTicker(detachJobPollEvery)
	defer ticker.Stop()

	current := job
	jobGone := false
waitLoop:
	for {
		if allLogicalJobTerminal(current) {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			break waitLoop
		case <-ticker.C:
			next, errs, resolveErr := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
			if resolveErr != nil {
				// errDetachJobNotFound here means no metadata is visible
				// anywhere — wrapper exited and cleaned up, or all pods
				// concurrently dropped the file. Treat as terminal.
				if errors.Is(resolveErr, errDetachJobNotFound) {
					for _, e := range errs {
						aggregated[e.Pod] = e.Error
					}
					jobGone = true
					break waitLoop
				}
				printExecJobsErrors(out, mergePodErrors(aggregated, nil))
				return resolveErr
			}
			current = next
			for _, e := range errs {
				aggregated[e.Pod] = e.Error
			}
			// Note: deliberately do NOT break on transient pod-query errors.
			// The deadline already bounds total wait time; bailing here on a
			// flaky poll left SIGKILL operating on a stale snapshot.
		}
	}

	killErrors := make([]execJobsPodError, 0)
	if !jobGone && !allLogicalJobTerminal(current) {
		for _, row := range current.PodStates {
			if row.State != "running" {
				continue
			}
			result, signalErr := signalDetachJob(ctx, client, namespace, row, "KILL")
			if signalErr != nil {
				killErrors = append(killErrors, execJobsPodError{Pod: row.Pod, Error: signalErr.Error()})
				fmt.Fprintf(out, "%s error: %v\n", prefixByPod[row.Pod], signalErr)
				continue
			}
			fmt.Fprintf(out, "%s sent SIGKILL pid=%d (%s)\n", prefixByPod[row.Pod], row.PID, result)
		}
	}
	for _, e := range killErrors {
		aggregated[e.Pod] = e.Error
	}

	terminalOk := jobGone
	if !jobGone {
		final, finalPodErrors, finalErr := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
		if finalErr != nil {
			// not-found at this point means the wrapper finished and no
			// metadata is visible — treat as terminal so a successful stop
			// isn't reported as a failure.
			if !errors.Is(finalErr, errDetachJobNotFound) {
				printExecJobsErrors(out, mergePodErrors(aggregated, nil))
				return finalErr
			}
			for _, e := range finalPodErrors {
				aggregated[e.Pod] = e.Error
			}
			terminalOk = true
		} else {
			for _, e := range finalPodErrors {
				aggregated[e.Pod] = e.Error
			}
			terminalOk = allLogicalJobTerminal(final)
			current = final
		}
	}

	allErrors := mergePodErrors(aggregated, nil)
	printExecJobsErrors(out, allErrors)
	if len(allErrors) > 0 {
		return fmt.Errorf("failed to stop detached job %q on %d pod(s)", jobID, len(allErrors))
	}
	if !terminalOk {
		return fmt.Errorf("detached job %q still has running pods", jobID)
	}
	return nil
}

func resolveLogicalDetachJob(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int) (logicalExecJobView, []execJobsPodError, error) {
	rows, podErrors := collectDetachJobs(ctx, client, namespace, pods, container, jobID, fanout)
	jobs := groupDetachJobs(rows)
	if len(jobs) == 0 {
		if len(podErrors) > 0 {
			return logicalExecJobView{}, podErrors, fmt.Errorf("%w %q (%d of %d pod(s) failed to query)", errDetachJobNotFound, jobID, len(podErrors), len(pods))
		}
		return logicalExecJobView{}, podErrors, fmt.Errorf("%w %q", errDetachJobNotFound, jobID)
	}
	return jobs[0], podErrors, nil
}

// mergePodErrors deduplicates per-pod error entries (last write wins) and
// returns a sorted slice suitable for printExecJobsErrors / FAILED footer.
func mergePodErrors(podErrors map[string]string, extras []execJobsPodError) []execJobsPodError {
	merged := make(map[string]string, len(podErrors)+len(extras))
	for k, v := range podErrors {
		merged[k] = v
	}
	for _, e := range extras {
		merged[e.Pod] = e.Error
	}
	out := make([]execJobsPodError, 0, len(merged))
	for pod, msg := range merged {
		out = append(out, execJobsPodError{Pod: pod, Error: msg})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Pod < out[j].Pod })
	return out
}

func jobPodNames(job logicalExecJobView) []string {
	names := make([]string, 0, len(job.PodStates))
	for _, row := range job.PodStates {
		names = append(names, row.Pod)
	}
	return names
}

func allLogicalJobTerminal(job logicalExecJobView) bool {
	if len(job.PodStates) == 0 {
		return false
	}
	for _, row := range job.PodStates {
		if row.State == "running" {
			return false
		}
	}
	return true
}

func logicalJobFailed(job logicalExecJobView) bool {
	for _, row := range job.PodStates {
		if row.State == "orphaned" {
			return true
		}
		if row.ExitCode != nil && *row.ExitCode != 0 {
			return true
		}
		if row.State != "exited" {
			return true
		}
	}
	return false
}

func streamDetachJobLog(ctx context.Context, client detachJobClient, namespace string, row execJobView, prefix string, follow bool, out io.Writer, mu *sync.Mutex) error {
	script := readDetachJobLogScript(row.LogPath)
	if follow {
		script = followDetachJobLogScript(row.LogPath)
	}
	var stderr bytes.Buffer
	writer := newPrefixedWriter(prefix, out, mu)
	defer writer.Flush()
	err := client.StreamShInContainer(ctx, namespace, row.Pod, row.Container, script, writer, &stderr)
	if err != nil && strings.TrimSpace(stderr.String()) != "" {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return err
}

func readDetachJobLogScript(logPath string) string {
	quoted := shellQuote(logPath)
	return fmt.Sprintf("if [ ! -f %s ]; then echo 'missing detached job log' >&2; exit 1; fi\ncat %s\n", quoted, quoted)
}

func followDetachJobLogScript(logPath string) string {
	quoted := shellQuote(logPath)
	return fmt.Sprintf("if [ ! -f %s ]; then echo 'missing detached job log' >&2; exit 1; fi\nexec tail -n +1 -f %s\n", quoted, quoted)
}

func signalDetachJob(ctx context.Context, client detachJobClient, namespace string, row execJobView, signalName string) (string, error) {
	out, err := client.ExecShInContainer(ctx, namespace, row.Pod, row.Container, signalDetachJobScript(row.JobID, row.PID, signalName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func signalDetachJobScript(jobID string, pid int, signalName string) string {
	return fmt.Sprintf(
		"pid=%d\n"+
			"env_path=\"/proc/$pid/environ\"\n"+
			"if [ ! -r \"$env_path\" ]; then\n"+
			"  echo not-running\n"+
			"  exit 0\n"+
			"fi\n"+
			"if ! tr '\\000' '\\n' <\"$env_path\" | grep -Fqx %s; then\n"+
			"  echo job-mismatch >&2\n"+
			"  exit 1\n"+
			"fi\n"+
			"if kill -%s \"$pid\" 2>/dev/null; then\n"+
			"  echo signaled\n"+
			"  exit 0\n"+
			"fi\n"+
			"echo not-running\n",
		pid,
		shellQuote("OKDEV_JOB_ID="+jobID),
		signalName,
	)
}

func printExecJobsErrors(out io.Writer, errs []execJobsPodError) {
	if len(errs) == 0 {
		return
	}
	fmt.Fprintf(out, "\nFAILED:\n")
	for _, pe := range errs {
		fmt.Fprintf(out, "pod=%s error=%q\n", pe.Pod, pe.Error)
	}
}
