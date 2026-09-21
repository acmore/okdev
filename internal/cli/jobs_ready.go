package cli

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
)

type jobsReadyOptions struct {
	Probe        string
	ProbeTimeout time.Duration
	PollInterval time.Duration
}

func newJobsReadyCmd(opts *Options) *cobra.Command {
	var probe, container, role string
	var pods, labels, exclude []string
	var timeout, probeTimeout time.Duration
	cmd := &cobra.Command{
		Use:   "ready <job-id> [session]",
		Short: "Wait for a running detached job's health probe to return its job ID",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			if strings.TrimSpace(args[0]) == "" {
				return fmt.Errorf("a non-empty job ID is required")
			}
			if strings.TrimSpace(probe) == "" {
				return fmt.Errorf("--probe is required; it must check health and print the service's job ID")
			}
			if timeout <= 0 || probeTimeout <= 0 {
				return fmt.Errorf("--timeout and --probe-timeout must be positive")
			}
			applySessionArg(opts, args[1:])
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			ctx, cancel := context.WithTimeout(cmd.Context(), timeout)
			defer cancel()
			if err := ensureSessionAccess(cc.opts, cc.kube, cc.namespace, cc.sessionName, true, ctx); err != nil {
				return err
			}
			selected, err := selectSessionPods(ctx, cc, pods, role, labels, exclude, true)
			if err != nil {
				return err
			}
			target := strings.TrimSpace(container)
			if target == "" {
				target = resolveTargetContainer(cc.cfg)
			}
			id := strings.TrimSpace(args[0])
			job, err := runJobsReady(ctx, cc.kube, cc.namespace, selected, target, id, jobsReadyOptions{Probe: probe, ProbeTimeout: probeTimeout, PollInterval: time.Second})
			if err != nil {
				return err
			}
			if cc.opts.Output == "json" {
				return outputJSON(cmd.OutOrStdout(), struct {
					JobID string   `json:"jobId"`
					Ready bool     `json:"ready"`
					Pods  []string `json:"pods"`
				}{id, true, jobPodNames(job)})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "job %s ready on %d pod(s); health and job identity verified\n", id, len(job.PodStates))
			return nil
		},
	}
	cmd.Flags().StringVar(&probe, "probe", "", "Read-only shell probe in the job container; stdout must equal the job ID")
	cmd.Flags().DurationVar(&timeout, "timeout", 2*time.Minute, "Total readiness wait deadline")
	cmd.Flags().DurationVar(&probeTimeout, "probe-timeout", 5*time.Second, "Deadline for each probe invocation")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().StringSliceVar(&pods, "pod", nil, "Check only these pods (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Check only pods with this workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Check only pods matching label key=value")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Ignore these pods")
	return cmd
}

// Keep probe output bounded; it is an identity token, not a log stream.
type readinessOutput struct {
	data     []byte
	overflow bool
}

func (w *readinessOutput) Write(p []byte) (int, error) {
	n := len(p)
	remaining := 4096 - len(w.data)
	if n > remaining {
		w.overflow = true
		p = p[:remaining]
	}
	w.data = append(w.data, p...)
	return n, nil
}

func readyJobMembers(job logicalExecJobView) error {
	if len(job.PodStates) == 0 {
		return fmt.Errorf("job %q has no tracked members", job.JobID)
	}
	for _, member := range job.PodStates {
		if member.State != "running" || member.PID <= 0 {
			return fmt.Errorf("job %q is not running on pod %s (state %s); readiness refused", job.JobID, member.Pod, member.State)
		}
	}
	return nil
}

func sameReadyJob(before, after logicalExecJobView) bool {
	if before.JobID != after.JobID || len(before.PodStates) != len(after.PodStates) {
		return false
	}
	for _, old := range before.PodStates {
		found := false
		for _, current := range after.PodStates {
			if old.Pod == current.Pod && old.Container == current.Container && old.PID == current.PID && old.StartedAt == current.StartedAt {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

func runJobsReady(ctx context.Context, client detachJobClient, namespace string, pods []kube.PodSummary, container, id string, options jobsReadyOptions) (logicalExecJobView, error) {
	if strings.TrimSpace(id) == "" {
		return logicalExecJobView{}, fmt.Errorf("a non-empty job ID is required")
	}
	last := "probe has not passed"
	query := func() (logicalExecJobView, error) {
		job, failures, err := resolveLogicalDetachJob(ctx, client, namespace, pods, container, id, pdshDefaultFanout)
		if ctx.Err() != nil {
			return job, fmt.Errorf("job %q readiness: %s: %w", id, last, ctx.Err())
		}
		if err != nil {
			return job, err
		}
		if len(failures) > 0 {
			return job, fmt.Errorf("cannot verify job %q on %d pod(s)", id, len(failures))
		}
		return job, readyJobMembers(job)
	}
	original, err := query()
	if err != nil {
		return original, err
	}
	for {
		if ctx.Err() != nil {
			return original, fmt.Errorf("job %q readiness: %s: %w", id, last, ctx.Err())
		}
		before, err := query()
		if err != nil {
			return before, err
		}
		if !sameReadyJob(original, before) {
			return before, fmt.Errorf("job %q membership or process identity changed", id)
		}
		allReady := true
		for _, member := range before.PodStates {
			probeCtx, cancel := context.WithTimeout(ctx, options.ProbeTimeout)
			var output readinessOutput
			err := client.StreamShInContainer(probeCtx, namespace, member.Pod, member.Container, options.Probe, &output, io.Discard)
			expired := probeCtx.Err() != nil
			cancel()
			if output.overflow {
				allReady = false
				last = "probe output exceeded 4096 bytes instead of the expected job ID on pod " + member.Pod
			} else if err == nil && strings.TrimSpace(string(output.data)) != id {
				allReady = false
				last = "probe did not return the expected job ID on pod " + member.Pod
			} else if err != nil || expired {
				allReady = false
				last = "probe failed or timed out on pod " + member.Pod
			}
			observed, err := query()
			if err != nil {
				return observed, err
			}
			if !sameReadyJob(original, observed) {
				return observed, fmt.Errorf("job %q membership or process identity changed", id)
			}
		}
		after, err := query()
		if err != nil {
			return after, err
		}
		if !sameReadyJob(original, after) {
			return after, fmt.Errorf("job %q membership or process identity changed", id)
		}
		if allReady && ctx.Err() == nil {
			return after, nil
		}
		timer := time.NewTimer(options.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return after, fmt.Errorf("job %q readiness: %s: %w", id, last, ctx.Err())
		case <-timer.C:
		}
	}
}
