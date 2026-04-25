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
	"github.com/acmore/okdev/internal/output"
	"github.com/spf13/cobra"
)

func newExecJobsCmd(opts *Options) *cobra.Command {
	var podNames []string
	var role string
	var labels []string
	var exclude []string
	var container string
	var jobID string
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
			return runExecJobs(cmd.Context(), cc.kube, cc.namespace, pods, targetContainer, jobID, effectiveFanout, cmd.OutOrStdout(), strings.EqualFold(opts.Output, "json"))
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

// execJobsOutput is the JSON shape for `okdev exec-jobs`. Per-pod errors
// are carried alongside the successful job rows so operators can see both
// results from one listing instead of losing everything on the first
// failure.
type execJobsOutput struct {
	Jobs   []execJobView      `json:"jobs"`
	Errors []execJobsPodError `json:"errors,omitempty"`
}

type execJobsPodError struct {
	Pod   string `json:"pod"`
	Error string `json:"error"`
}

func runExecJobs(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, jobID string, fanout int, out io.Writer, jsonOutput bool) error {
	rows, podErrors := collectDetachJobs(ctx, client, namespace, pods, container, jobID, fanout)
	if jsonOutput {
		if err := outputJSON(out, execJobsOutput{Jobs: rows, Errors: podErrors}); err != nil {
			return err
		}
	} else {
		if len(rows) == 0 && len(podErrors) == 0 {
			fmt.Fprintln(out, "No detached exec jobs found")
		}
		if len(rows) > 0 {
			table := make([][]string, 0, len(rows))
			for _, row := range rows {
				exit := "-"
				if row.ExitCode != nil {
					exit = fmt.Sprintf("%d", *row.ExitCode)
				}
				table = append(table, []string{
					row.Pod,
					row.Container,
					row.JobID,
					fmt.Sprintf("%d", row.PID),
					row.State,
					exit,
					row.LogPath,
				})
			}
			output.PrintTable(out, []string{"POD", "CONTAINER", "JOB ID", "PID", "STATE", "EXIT", "LOG"}, table)
		}
		if len(podErrors) > 0 {
			fmt.Fprintf(out, "\nFAILED:\n")
			for _, pe := range podErrors {
				fmt.Fprintf(out, "pod=%s error=%q\n", pe.Pod, pe.Error)
			}
		}
	}
	if len(podErrors) > 0 {
		return fmt.Errorf("%d of %d pods failed to list detached exec jobs", len(podErrors), len(pods))
	}
	return nil
}

// collectDetachJobs fans out the listing command to every pod and returns
// all successful job views plus a per-pod error list. It does NOT short-
// circuit on the first failure: a single flaky pod should not hide jobs
// running healthily on the rest of the fleet.
func collectDetachJobs(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, jobID string, fanout int) ([]execJobView, []execJobsPodError) {
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
	podErrors := make([]execJobsPodError, 0)
	for range pods {
		r := <-results
		if r.err != nil {
			podErrors = append(podErrors, execJobsPodError{Pod: r.pod, Error: r.err.Error()})
			continue
		}
		for _, job := range r.jobs {
			if strings.TrimSpace(jobID) != "" && job.JobID != jobID {
				continue
			}
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
	sort.Slice(podErrors, func(i, j int) bool { return podErrors[i].Pod < podErrors[j].Pod })
	return rows, podErrors
}

func detachJobsCommand() []string {
	// Each output line is "<alive>\t<json>" where <alive> is 1 if the job's
	// pid is still running and its /proc/<pid>/environ contains the
	// OKDEV_JOB_ID we set in the wrapper. Matching on an environment
	// variable (rather than cmdline) is robust against PID reuse even when
	// the job's cmdline is the arbitrary user command. The find(1) call
	// also reaps stale .tmp files left behind by killed launchers/wrappers.
	script := `dir='` + detachMetadataDir + `'
[ -d "$dir" ] || exit 0
find "$dir" -maxdepth 1 -name '*.tmp' -type f -mmin +1 -delete 2>/dev/null || true
for f in "$dir"/*.json; do
  [ -e "$f" ] || continue
  contents=$(cat "$f")
  pid=$(printf '%s' "$contents" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' | head -n1)
  job_id=$(basename "$f" .json)
  alive=0
  if [ -n "$pid" ] && [ -r "/proc/$pid/environ" ]; then
    if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -qx "OKDEV_JOB_ID=$job_id"; then
      alive=1
    fi
  fi
  printf '%s\t%s\n' "$alive" "$contents"
done
`
	return []string{"sh", "-c", script}
}

func parseDetachMetadataLines(raw string) ([]detachMetadata, error) {
	lines := strings.Split(raw, "\n")
	out := make([]detachMetadata, 0)
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Lines from detachJobsCommand are "<alive>\t<json>"; tolerate
		// raw JSON for older containers that ran a previous cli version.
		alive := true
		if tab := strings.IndexByte(line, '\t'); tab >= 0 {
			alive = line[:tab] != "0"
			line = line[tab+1:]
		}
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
		if !alive && job.State == "running" {
			job.State = "orphaned"
		}
		out = append(out, job)
	}
	return out, nil
}
