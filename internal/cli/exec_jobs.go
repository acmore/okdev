package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/connect"
	"github.com/acmore/okdev/internal/kube"
	"github.com/spf13/cobra"
)

func newExecJobsCmd(opts *Options) *cobra.Command {
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var container string
	var fanout int
	var readyOnly bool

	cmd := &cobra.Command{
		Use:               "exec-jobs [session]",
		Short:             "List detached exec jobs",
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

			pods, err := selectExecPods(cmd.Context(), cc, podNames, role, labels, exclude, readyOnly)
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
			return runExecJobs(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, effectiveFanout, cmd.OutOrStdout(), strings.EqualFold(opts.Output, "json"))
		},
	}
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Target specific pods by name (repeatable/comma-separated)")
	cmd.Flags().StringVar(&role, "role", "", "Target pods by workload role")
	cmd.Flags().StringSliceVar(&labels, "label", nil, "Target pods by label key=value (repeatable)")
	cmd.Flags().StringSliceVar(&exclude, "exclude", nil, "Exclude specific pods (repeatable/comma-separated)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	cmd.Flags().IntVar(&fanout, "fanout", pdshDefaultFanout, "Maximum concurrent pod queries")
	cmd.Flags().BoolVar(&readyOnly, "ready-only", false, "Query only pods that are already running (skip readiness check)")
	return cmd
}

func selectExecPods(ctx context.Context, cc *commandContext, podNames []string, role string, labels []string, exclude []string, readyOnly bool) ([]kube.PodSummary, error) {
	labelSel := selectorForSessionRun(cc.sessionName)
	sessionPods, err := cc.kube.ListPods(ctx, cc.namespace, false, labelSel)
	if err != nil {
		return nil, fmt.Errorf("list session pods: %w", err)
	}
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

type execJobView struct {
	Pod       string `json:"pod"`
	Container string `json:"container"`
	JobID     string `json:"jobId"`
	PID       int    `json:"pid"`
	State     string `json:"state"`
	ExitCode  *int   `json:"exitCode,omitempty"`
	LogPath   string `json:"logPath"`
	MetaPath  string `json:"metaPath"`
	StartedAt string `json:"startedAt"`
	Command   string `json:"command"`
}

type podDetachJobsResult struct {
	pod  string
	jobs []detachMetadata
	err  error
}

func runExecJobs(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, fanout int, out io.Writer, jsonOutput bool) error {
	rows, err := collectDetachJobs(ctx, client, namespace, pods, container, fanout)
	if err != nil {
		return err
	}
	if jsonOutput {
		return outputJSON(out, rows)
	}
	if len(rows) == 0 {
		fmt.Fprintln(out, "No detached exec jobs found")
		return nil
	}
	for _, row := range rows {
		exit := "-"
		if row.ExitCode != nil {
			exit = fmt.Sprintf("%d", *row.ExitCode)
		}
		fmt.Fprintf(out, "pod=%s container=%s job_id=%s pid=%d state=%s exit=%s log=%s meta=%s\n",
			row.Pod, row.Container, row.JobID, row.PID, row.State, exit, row.LogPath, row.MetaPath)
	}
	return nil
}

func collectDetachJobs(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, fanout int) ([]execJobView, error) {
	results := make(chan podDetachJobsResult, len(pods))
	sem := make(chan struct{}, fanout)
	for _, pod := range pods {
		pod := pod
		go func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			var stdout bytes.Buffer
			err := connect.RunOnContainer(ctx, client, namespace, pod.Name, container, detachJobsCommand(), false, nil, &stdout, io.Discard)
			if err != nil {
				results <- podDetachJobsResult{pod: pod.Name, err: err}
				return
			}
			jobs, err := parseDetachMetadataLines(stdout.String())
			results <- podDetachJobsResult{pod: pod.Name, jobs: jobs, err: err}
		}()
	}
	rows := make([]execJobView, 0)
	for range pods {
		r := <-results
		if r.err != nil {
			return nil, fmt.Errorf("list detached exec jobs on pod %s: %w", r.pod, r.err)
		}
		for _, job := range r.jobs {
			containerLabel := job.Container
			if strings.TrimSpace(containerLabel) == "" {
				containerLabel = container
			}
			rows = append(rows, execJobView{
				Pod:       job.Pod,
				Container: containerLabel,
				JobID:     job.JobID,
				PID:       job.PID,
				State:     job.State,
				ExitCode:  job.ExitCode,
				LogPath:   job.StdoutPath,
				MetaPath:  job.MetaPath,
				StartedAt: job.StartedAt,
				Command:   job.Command,
			})
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Pod != rows[j].Pod {
			return rows[i].Pod < rows[j].Pod
		}
		if rows[i].StartedAt != rows[j].StartedAt {
			return rows[i].StartedAt < rows[j].StartedAt
		}
		return rows[i].JobID < rows[j].JobID
	})
	return rows, nil
}

func detachJobsCommand() []string {
	return []string{"sh", "-c", "dir='" + detachMetadataDir + "'; [ -d \"$dir\" ] || exit 0; for f in \"$dir\"/*.json; do [ -e \"$f\" ] || continue; cat \"$f\"; printf '\\n'; done"}
}

func parseDetachMetadataLines(raw string) ([]detachMetadata, error) {
	lines := strings.Split(raw, "\n")
	out := make([]detachMetadata, 0)
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var job detachMetadata
		if err := json.Unmarshal([]byte(line), &job); err != nil {
			return nil, fmt.Errorf("parse detach job metadata: %w", err)
		}
		if job.JobID == "" || job.Pod == "" || job.PID <= 0 || job.MetaPath == "" {
			return nil, errors.New("missing detach job metadata")
		}
		if strings.TrimSpace(job.State) == "" {
			job.State = "unknown"
		}
		out = append(out, job)
	}
	return out, nil
}
