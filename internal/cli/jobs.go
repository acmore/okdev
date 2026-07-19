package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
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
	var tailLines int
	var since string
	var grep string
	var dedup bool
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
			if tailLines < -1 {
				return fmt.Errorf("--tail must be -1 (all), 0, or a positive line count")
			}
			logOpts := jobsLogsOptions{Follow: follow, TailLines: tailLines, Grep: grep, Dedup: dedup}
			if follow && (strings.TrimSpace(grep) != "" || dedup) {
				return fmt.Errorf("--grep and --dedup apply to snapshot reads and cannot be used with --follow")
			}
			if strings.TrimSpace(since) != "" {
				if follow {
					return fmt.Errorf("--since cannot be used with --follow")
				}
				cutoff, err := parseSinceCutoff(since, time.Now())
				if err != nil {
					return err
				}
				logOpts.SinceEpoch = cutoff.Unix()
			}
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
			return runJobsLogs(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, strings.TrimSpace(args[0]), fanoutOrDefault(fanout), logOpts, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Follow logs until all pods in the job finish")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	cmd.Flags().IntVar(&tailLines, "tail", -1, "Show only the last N lines of each pod's log (-1 = all)")
	cmd.Flags().StringVar(&since, "since", "", "Skip pods whose log file has not changed within this duration (e.g. 90s, 5m) or since an RFC3339 time; file-level gate, output is still the (tail-limited) current log")
	cmd.Flags().StringVar(&grep, "grep", "", "Keep only lines matching this extended regex (pod-side, after CR normalization, before --tail)")
	cmd.Flags().BoolVar(&dedup, "dedup", false, "Fold consecutive identical lines into one copy plus a [repeated Nx] count")
	return cmd
}

// jobsLogsOptions carries the log-read shaping for jobs logs (#167): --tail
// and --since cut the cost of high-frequency poll loops by ~2 orders of
// magnitude versus re-fetching the whole file each round; --grep and --dedup
// (#188/#189) shrink what one read costs an automated caller.
type jobsLogsOptions struct {
	Follow    bool
	TailLines int // -1 = whole file
	// SinceEpoch, when >0, skips pods whose log file mtime is older. The
	// job log stream carries no per-line timestamps, so this is a
	// file-level activity gate, not a per-line filter.
	SinceEpoch int64
	// Grep, when non-empty, keeps only lines matching this extended regex
	// (applied pod-side, after CR normalization, before --tail).
	Grep string
	// Dedup folds consecutive identical lines into one plus a count.
	Dedup bool
}

// parseSinceCutoff accepts a relative duration ("90s", "5m") or an absolute
// RFC3339 time and returns the cutoff instant.
func parseSinceCutoff(s string, now time.Time) (time.Time, error) {
	if d, err := time.ParseDuration(s); err == nil {
		if d < 0 {
			return time.Time{}, fmt.Errorf("--since duration must be positive, got %q", s)
		}
		return now.Add(-d), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	return time.Time{}, fmt.Errorf("--since %q is neither a duration (90s, 5m) nor an RFC3339 time", s)
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
	var grep string
	var tailLines int
	cmd := &cobra.Command{
		Use:   "wait <job-id> [session]",
		Short: "Wait for a detached job to finish, or for a log pattern to appear",
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
			return runJobsWait(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, strings.TrimSpace(args[0]), fanoutOrDefault(fanout), jobsWaitOptions{Grep: grep, TailLines: tailLines}, cmd.OutOrStdout())
		},
	}
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	cmd.Flags().StringVar(&grep, "grep", "", "Return as soon as a line matching this extended regex appears in any pod's log (instead of waiting for the job to finish)")
	cmd.Flags().IntVar(&tailLines, "tail", 1, "With --grep: how many matching lines to print on success (-1 = all matches)")
	return cmd
}

func fanoutOrDefault(v int) int {
	if v <= 0 {
		return pdshDefaultFanout
	}
	return v
}

func runJobsLogs(ctx context.Context, client detachJobClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int, logOpts jobsLogsOptions, out io.Writer) error {
	follow := logOpts.Follow
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
			err := streamDetachJobLog(streamCtx, client, namespace, row, prefixByPod[row.Pod], logOpts, out, &writeMu)
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

// jobsWaitOptions shapes jobs wait: with Grep set, the wait returns as soon
// as the pattern appears in any pod's log — the core monitoring primitive
// ("return once we reach step 2 / an Error shows up", #191) — instead of
// blocking until the job exits.
type jobsWaitOptions struct {
	Grep      string
	TailLines int // matching lines to print on success (-1 = all matches)
}

func runJobsWait(ctx context.Context, client detachJobClient, namespace string, pods []kube.PodSummary, container, jobID string, fanout int, waitOpts jobsWaitOptions, out io.Writer) error {
	ticker := time.NewTicker(detachJobPollEvery)
	defer ticker.Stop()
	waitForPattern := strings.TrimSpace(waitOpts.Grep) != ""
	var lastProbeErr error
	for {
		job, podErrors, err := resolveLogicalDetachJob(ctx, client, namespace, pods, container, jobID, fanout)
		if err != nil {
			return err
		}
		if len(podErrors) > 0 {
			return fmt.Errorf("failed to query detached job %q on %d pod(s)", jobID, len(podErrors))
		}
		if waitForPattern {
			// Probe before the terminal check so a job that exits with the
			// pattern already in its log (including waiting for an Error)
			// still counts as matched.
			matched, probeErr := probeDetachJobLogPattern(ctx, client, namespace, job, waitOpts, out)
			if probeErr != nil {
				// Transient read failures must not abort a long wait; they
				// only matter if the job ends without ever matching.
				lastProbeErr = probeErr
			}
			if matched {
				return nil
			}
		}
		if allLogicalJobTerminal(job) {
			if waitForPattern {
				msg := fmt.Errorf("detached job %q finished with state %s without matching --grep %q", jobID, job.SummaryState, waitOpts.Grep)
				if lastProbeErr != nil {
					return fmt.Errorf("%w (last log probe error: %v)", msg, lastProbeErr)
				}
				return msg
			}
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

// probeDetachJobLogPattern greps every pod's log through the filter pipeline
// and, on the first pod with matching lines, prints them (pod-prefixed) and
// reports success.
func probeDetachJobLogPattern(ctx context.Context, client detachJobClient, namespace string, job logicalExecJobView, waitOpts jobsWaitOptions, out io.Writer) (bool, error) {
	shortNames := shortPodNames(jobPodNames(job))
	prefixes := formatPodPrefixes(shortNames, false)
	var lastErr error
	for i, row := range job.PodStates {
		content, err := readDetachJobLogVerified(ctx, client, namespace, row, jobsLogsOptions{
			TailLines: waitOpts.TailLines,
			Grep:      waitOpts.Grep,
		})
		if err != nil {
			lastErr = err
			continue
		}
		if strings.TrimSpace(content) == "" {
			continue
		}
		var mu sync.Mutex
		writer := newPrefixedWriter(prefixes[i], out, &mu)
		_, _ = writer.Write([]byte(content))
		writer.Flush()
		return true, nil
	}
	return false, lastErr
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
		if !stopRowNeedsSignal(row) {
			continue
		}
		result, signalErr := signalDetachJob(ctx, client, namespace, row, "TERM")
		if signalErr != nil {
			signalErrors = append(signalErrors, execJobsPodError{Pod: row.Pod, Error: signalErr.Error()})
			fmt.Fprintf(out, "%s error: %v\n", prefixByPod[row.Pod], signalErr)
			continue
		}
		fmt.Fprintf(out, "%s sent SIGTERM %s (%s)\n", prefixByPod[row.Pod], stopSignalTargetLabel(row), result)
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
		if stopJobSettled(current) {
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
	if !jobGone && !stopJobSettled(current) {
		for _, row := range current.PodStates {
			if !stopRowNeedsSignal(row) {
				continue
			}
			result, signalErr := signalDetachJob(ctx, client, namespace, row, "KILL")
			if signalErr != nil {
				killErrors = append(killErrors, execJobsPodError{Pod: row.Pod, Error: signalErr.Error()})
				fmt.Fprintf(out, "%s error: %v\n", prefixByPod[row.Pod], signalErr)
				continue
			}
			fmt.Fprintf(out, "%s sent SIGKILL %s (%s)\n", prefixByPod[row.Pod], stopSignalTargetLabel(row), result)
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
			terminalOk = stopJobSettled(final)
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

// stopRowNeedsSignal reports whether `jobs stop` should signal this pod's
// record: the job still runs, or the leader exited but verified group
// members linger (torchrun children holding GPU memory).
func stopRowNeedsSignal(row execJobView) bool {
	return row.State == "running" || (row.PGID > 0 && row.GroupLive > 0)
}

// stopJobSettled is `jobs stop`'s terminal criterion: every pod record is
// terminal AND no live group members remain. Stricter than
// allLogicalJobTerminal (used by logs/wait), which only tracks the leader's
// metadata state.
func stopJobSettled(job logicalExecJobView) bool {
	if len(job.PodStates) == 0 {
		return false
	}
	for _, row := range job.PodStates {
		if stopRowNeedsSignal(row) {
			return false
		}
	}
	return true
}

func stopSignalTargetLabel(row execJobView) string {
	if row.PGID > 0 {
		return fmt.Sprintf("pgid=%d", row.PGID)
	}
	return fmt.Sprintf("pid=%d", row.PID)
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

// detachLogReadAttempts bounds the read retries when the received byte count
// does not match the size the read script reported (#167: apiserver exec
// streams occasionally drop output, surfacing as empty/truncated logs for a
// job whose log file is demonstrably non-empty).
const detachLogReadAttempts = 3

func streamDetachJobLog(ctx context.Context, client detachJobClient, namespace string, row execJobView, prefix string, logOpts jobsLogsOptions, out io.Writer, mu *sync.Mutex) error {
	if logOpts.Follow {
		script := followDetachJobLogScript(row.LogPath, logOpts.TailLines)
		var stderr bytes.Buffer
		writer := newPrefixedWriter(prefix, out, mu)
		defer writer.Flush()
		err := streamDetachLogWithSidecarFallback(ctx, client, namespace, row, script, writer, &stderr)
		if err != nil && strings.TrimSpace(stderr.String()) != "" {
			return fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
		}
		return err
	}

	// Non-follow reads buffer the whole (tail-limited) selection and verify
	// it against the byte count the script reported before emitting, so a
	// dropped exec stream is retried instead of being presented as the job's
	// (empty) output.
	content, err := readDetachJobLogVerified(ctx, client, namespace, row, logOpts)
	if err != nil {
		return err
	}
	writer := newPrefixedWriter(prefix, out, mu)
	defer writer.Flush()
	if _, err := writer.Write([]byte(content)); err != nil {
		return err
	}
	return nil
}

// readDetachJobLogVerified performs one size-verified snapshot read of a
// pod's job log (retrying dropped exec streams, #167) and returns the
// selected content.
func readDetachJobLogVerified(ctx context.Context, client detachJobClient, namespace string, row execJobView, logOpts jobsLogsOptions) (string, error) {
	script := readDetachJobLogScript(row.LogPath, logOpts)
	var lastErr error
	for attempt := 1; attempt <= detachLogReadAttempts; attempt++ {
		var buf, stderr bytes.Buffer
		err := streamDetachLogWithSidecarFallback(ctx, client, namespace, row, script, &buf, &stderr)
		if err != nil {
			if strings.TrimSpace(stderr.String()) != "" {
				return "", fmt.Errorf("%w: %s", err, strings.TrimSpace(stderr.String()))
			}
			return "", err
		}
		want, ok := parseDetachLogSize(stderr.String())
		if !ok || int64(buf.Len()) != want {
			got := int64(buf.Len())
			if !ok {
				lastErr = fmt.Errorf("log read stream dropped its size marker (got %d bytes) after %d attempt(s)", got, attempt)
			} else {
				lastErr = fmt.Errorf("log read truncated: got %d of %d bytes after %d attempt(s)", got, want, attempt)
			}
			continue
		}
		return buf.String(), nil
	}
	return "", lastErr
}

// streamDetachLogWithSidecarFallback runs the log script in the job's
// container, falling back to the always-running sidecar when that container
// is gone (OOMKilled, crashed) — logs under /var/okdev live on the shared
// runtime volume. Setup failures happen before any output is streamed, so the
// fallback cannot duplicate log lines.
func streamDetachLogWithSidecarFallback(ctx context.Context, client detachJobClient, namespace string, row execJobView, script string, stdout io.Writer, stderr *bytes.Buffer) error {
	err := client.StreamShInContainer(ctx, namespace, row.Pod, row.Container, script, stdout, stderr)
	if err != nil && row.Container != syncthingContainerName && isContainerUnavailableExecError(err) {
		stderr.Reset()
		err = client.StreamShInContainer(ctx, namespace, row.Pod, syncthingContainerName, script, stdout, stderr)
	}
	if err != nil && isContainerUnavailableExecError(err) {
		if hint := containerUnavailableHint(ctx, client, namespace, row.Pod, row.Container); hint != "" {
			err = fmt.Errorf("%w; %s", err, hint)
		}
	}
	return err
}

// readDetachJobLogScript snapshots the (tail-limited) selection into a temp
// file, reports its exact byte count on stderr (okdev-log-size:<n>), then
// emits it. The count lets the client detect a dropped or truncated exec
// stream and retry (#167). With sinceEpoch > 0, an unchanged log file is
// skipped entirely (okdev-log-size:0, nothing printed) — the fast path for
// poll loops.
func readDetachJobLogScript(logPath string, opts jobsLogsOptions) string {
	quoted := shellQuote(logPath)
	var b strings.Builder
	fmt.Fprintf(&b, "f=%s\n", quoted)
	b.WriteString("if [ ! -f \"$f\" ]; then echo 'missing detached job log' >&2; exit 1; fi\n")
	if opts.SinceEpoch > 0 {
		fmt.Fprintf(&b, "if [ \"$(stat -c %%Y \"$f\" 2>/dev/null || echo 0)\" -lt %d ]; then printf 'okdev-log-size:0\\n' >&2; exit 0; fi\n", opts.SinceEpoch)
	}
	b.WriteString("tmp=$(mktemp \"${TMPDIR:-/tmp}/okdev-log.XXXXXX\") || exit 1\n")
	b.WriteString("trap 'rm -f \"$tmp\"' EXIT\n")
	b.WriteString(detachLogPipeline(opts) + " >\"$tmp\"\n")
	b.WriteString("printf 'okdev-log-size:%s\\n' \"$(wc -c <\"$tmp\" | tr -d ' \\t')\" >&2\n")
	b.WriteString("cat \"$tmp\"\n")
	return b.String()
}

// detachLogPipeline builds the pod-side selection pipeline. Carriage returns
// are normalized first — progress bars (tqdm) rewrite one physical line with
// \r thousands of times, so a raw `tail -n N` would count that blob as one
// line and then explode downstream; CRLF endings are stripped rather than
// split. --grep then filters, --dedup folds consecutive identical lines into
// one plus a count, and --tail takes the newest N of whatever is left, so
// "latest N matches" composes the way poll loops expect. All commands are
// busybox-clean (sed, grep -E, awk, tail).
func detachLogPipeline(opts jobsLogsOptions) string {
	stages := []string{`sed -e 's/\r$//' -e 's/\r/\n/g' "$f"`}
	if strings.TrimSpace(opts.Grep) != "" {
		stages = append(stages, "grep -E -- "+shellQuote(opts.Grep))
	}
	if opts.Dedup {
		stages = append(stages, detachLogDedupStage)
	}
	if opts.TailLines >= 0 {
		stages = append(stages, fmt.Sprintf("tail -n %d", opts.TailLines))
	}
	return strings.Join(stages, " | ")
}

// detachLogDedupStage collapses runs of identical lines into a single copy
// suffixed with " [repeated Nx]" (#188): a multi-rank crash printing the same
// exception body per worker becomes one copy plus a count.
const detachLogDedupStage = `awk '` +
	`NR>1 && $0==prev {n++; next} ` +
	`{if (NR>1) {if (n>1) print prev " [repeated " n "x]"; else print prev} prev=$0; n=1} ` +
	`END {if (NR>0) {if (n>1) print prev " [repeated " n "x]"; else print prev}}'`

func followDetachJobLogScript(logPath string, tailLines int) string {
	quoted := shellQuote(logPath)
	from := "+1"
	if tailLines >= 0 {
		from = strconv.Itoa(tailLines)
	}
	return fmt.Sprintf("if [ ! -f %s ]; then echo 'missing detached job log' >&2; exit 1; fi\nexec tail -n %s -f %s\n", quoted, from, quoted)
}

// parseDetachLogSize extracts the okdev-log-size marker the read script
// prints on stderr.
func parseDetachLogSize(stderr string) (int64, bool) {
	for _, line := range strings.Split(stderr, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "okdev-log-size:") {
			continue
		}
		n, err := strconv.ParseInt(strings.TrimPrefix(line, "okdev-log-size:"), 10, 64)
		if err != nil {
			return 0, false
		}
		return n, true
	}
	return 0, false
}

func signalDetachJob(ctx context.Context, client detachJobClient, namespace string, row execJobView, signalName string) (string, error) {
	out, err := client.ExecShInContainer(ctx, namespace, row.Pod, row.Container, signalDetachJobScript(row.JobID, row.PID, row.PGID, signalName))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// signalDetachJobScript signals a detached job. With a process group (pgid >
// 0) it signals the whole group — covering children like torchrun workers
// whose leader may already be dead — after verifying via OKDEV_JOB_ID in a
// member's environ that the group still belongs to this job (a PGID number
// cannot be recycled while any member lives). Without a group, or when the
// group is already empty, it falls through to the legacy leader-only path.
func signalDetachJobScript(jobID string, pid, pgid int, signalName string) string {
	quotedJobEnv := shellQuote("OKDEV_JOB_ID=" + jobID)
	groupPrelude := ""
	if pgid > 0 {
		groupPrelude = fmt.Sprintf(
			"pgid=%d\n"+
				"members=$(awk -v g=\"$pgid\" '{ pid=$1; line=$0; sub(/^[0-9]+ \\(.*\\) /, \"\", line); split(line, f, \" \"); if (f[1] != \"Z\" && f[3] == g) print pid }' /proc/[0-9]*/stat 2>/dev/null)\n"+
				"for m in $members; do\n"+
				"  if tr '\\000' '\\n' < \"/proc/$m/environ\" 2>/dev/null | grep -Fqx %s; then\n"+
				// No `--` before the negative pgid: dash's kill builtin
				// rejects it ("Illegal number: -"); the bare "-<pgid>"
				// operand after the signal option works in dash, bash,
				// zsh, and busybox ash alike.
				"    if kill -%s \"-$pgid\" 2>/dev/null; then\n"+
				"      echo \"signaled group ($(set -- $members; echo $#) members)\"\n"+
				"      exit 0\n"+
				"    fi\n"+
				"    echo not-running\n"+
				"    exit 0\n"+
				"  fi\n"+
				"done\n",
			pgid,
			quotedJobEnv,
			signalName,
		)
	}
	return groupPrelude + fmt.Sprintf(
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
		quotedJobEnv,
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
