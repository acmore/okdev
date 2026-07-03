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
	"github.com/acmore/okdev/internal/fanout"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/output"
)

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

type logicalExecJobView struct {
	JobID        string        `json:"jobId"`
	SummaryState string        `json:"state"`
	Pods         int           `json:"pods"`
	StartedAt    string        `json:"startedAt"`
	Command      string        `json:"command"`
	PodStates    []execJobView `json:"podStates,omitempty"`
}

type podDetachJobsResult struct {
	pod  string
	jobs []detachMetadata
	err  error
}

// execJobsOutput is the JSON shape for `okdev jobs list` / `okdev exec-jobs`.
// Per-pod errors are carried alongside the successful grouped job rows so
// operators can see both results from one listing instead of losing
// everything on the first failure.
type execJobsOutput struct {
	Jobs   []logicalExecJobView `json:"jobs"`
	Errors []execJobsPodError   `json:"errors,omitempty"`
}

type execJobsPodError struct {
	Pod   string `json:"pod"`
	Error string `json:"error"`
}

func runExecJobs(ctx context.Context, client connect.ExecClient, namespace string, pods []kube.PodSummary, container string, jobID string, fanout int, out io.Writer, jsonOutput bool, route fanoutRoute) error {
	rows, podErrors, viaGateway := collectDetachJobsViaGateway(ctx, client, namespace, route, pods, container, jobID, fanout)
	if !viaGateway {
		rows, podErrors = collectDetachJobs(ctx, client, namespace, pods, container, jobID, fanout)
	}
	jobs := groupDetachJobs(rows)
	if jsonOutput {
		if err := outputJSON(out, execJobsOutput{Jobs: jobs, Errors: podErrors}); err != nil {
			return err
		}
	} else {
		if len(jobs) == 0 && len(podErrors) == 0 {
			fmt.Fprintln(out, "No detached exec jobs found")
		}
		if len(jobs) > 0 {
			table := make([][]string, 0, len(jobs))
			for _, job := range jobs {
				table = append(table, []string{
					job.JobID,
					job.SummaryState,
					fmt.Sprintf("%d", job.Pods),
					job.StartedAt,
					job.Command,
				})
			}
			output.PrintTable(out, []string{"JOB ID", "STATE", "PODS", "STARTED", "COMMAND"}, table)
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

// collectDetachJobsViaGateway lists detach jobs on all pods through the
// in-cluster fanout gateway: one apiserver exec instead of one per pod. The
// listing command is idempotent and read-only, so any gateway problem —
// ineligible session, no running gateway, stale driver, driver error —
// safely reports ok=false and lets the caller fall back to direct per-pod
// listing.
func collectDetachJobsViaGateway(ctx context.Context, client connect.ExecClient, namespace string, route fanoutRoute, pods []kube.PodSummary, container string, jobID string, fanoutN int) ([]execJobView, []execJobsPodError, bool) {
	if !useGatewayFanout(route, pods) {
		return nil, nil, false
	}
	gw, err := selectGatewayPod(pods, route.gatewayOverride)
	if err != nil {
		return nil, nil, false
	}
	targets := make([]fanout.Target, 0, len(pods))
	for _, pod := range pods {
		targets = append(targets, fanout.Target{Pod: pod.Name, Addr: pod.PodIP})
	}
	req := fanout.Request{
		Version: fanout.ProtocolVersion,
		User:    route.user,
		KeyPath: gatewayInterPodKeyPath,
		Port:    gatewaySSHPort,
		Targets: targets,
		Command: shellJoinArgv(detachJobsCommand()),
		Fanout:  fanoutN,
		Retries: gatewayFanoutRetries,
	}
	results, authoritative, err := runGatewayFanout(ctx, client, namespace, gw, container, req)
	if err != nil || !authoritative {
		return nil, nil, false
	}
	rows := make([]execJobView, 0)
	podErrors := make([]execJobsPodError, 0)
	for _, r := range results {
		if r.Status != fanout.StatusResponded {
			msg := r.Status
			if r.Error != "" {
				msg = r.Status + ": " + r.Error
			}
			podErrors = append(podErrors, execJobsPodError{Pod: r.Pod, Error: msg})
			continue
		}
		if r.Exit != 0 {
			podErrors = append(podErrors, execJobsPodError{Pod: r.Pod, Error: fmt.Sprintf("list command exited %d: %s", r.Exit, strings.TrimSpace(r.Stderr))})
			continue
		}
		jobs, err := parseDetachMetadataLines(r.Stdout)
		if err != nil {
			podErrors = append(podErrors, execJobsPodError{Pod: r.Pod, Error: err.Error()})
			continue
		}
		rows = appendDetachJobRows(rows, jobs, jobID, container)
	}
	sortDetachJobRows(rows)
	sort.Slice(podErrors, func(i, j int) bool { return podErrors[i].Pod < podErrors[j].Pod })
	return rows, podErrors, true
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
		rows = appendDetachJobRows(rows, r.jobs, jobID, container)
	}
	sortDetachJobRows(rows)
	sort.Slice(podErrors, func(i, j int) bool { return podErrors[i].Pod < podErrors[j].Pod })
	return rows, podErrors
}

func appendDetachJobRows(rows []execJobView, jobs []detachMetadata, jobID, container string) []execJobView {
	for _, job := range jobs {
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
	return rows
}

func sortDetachJobRows(rows []execJobView) {
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].Pod != rows[j].Pod {
			return rows[i].Pod < rows[j].Pod
		}
		if rows[i].StartedAt != rows[j].StartedAt {
			return rows[i].StartedAt < rows[j].StartedAt
		}
		return rows[i].JobID < rows[j].JobID
	})
}

func groupDetachJobs(rows []execJobView) []logicalExecJobView {
	byJob := make(map[string][]execJobView)
	for _, row := range rows {
		byJob[row.JobID] = append(byJob[row.JobID], row)
	}

	logical := make([]logicalExecJobView, 0, len(byJob))
	for jobID, members := range byJob {
		sort.Slice(members, func(i, j int) bool {
			if members[i].Pod != members[j].Pod {
				return members[i].Pod < members[j].Pod
			}
			return members[i].StartedAt < members[j].StartedAt
		})
		logical = append(logical, logicalExecJobView{
			JobID:        jobID,
			SummaryState: summarizeLogicalJobState(members),
			Pods:         len(members),
			StartedAt:    earliestStartedAt(members),
			Command:      members[0].Command,
			PodStates:    members,
		})
	}

	sort.Slice(logical, func(i, j int) bool {
		if logical[i].StartedAt != logical[j].StartedAt {
			return logical[i].StartedAt < logical[j].StartedAt
		}
		return logical[i].JobID < logical[j].JobID
	})
	return logical
}

func summarizeLogicalJobState(rows []execJobView) string {
	if len(rows) == 0 {
		return "unknown"
	}
	total := len(rows)
	var running, failed, exited int
	for _, row := range rows {
		switch {
		case row.State == "running":
			running++
		case row.State == "orphaned":
			failed++
		case row.ExitCode != nil && *row.ExitCode != 0:
			failed++
		case row.State == "exited":
			exited++
		}
	}
	if running > 0 {
		return fmt.Sprintf("running(%d/%d)", running, total)
	}
	if failed > 0 {
		return fmt.Sprintf("failed(%d/%d)", failed, total)
	}
	if exited == total {
		return fmt.Sprintf("exited(%d/%d)", exited, total)
	}
	return fmt.Sprintf("mixed(%d/%d)", total, total)
}

func earliestStartedAt(rows []execJobView) string {
	if len(rows) == 0 {
		return ""
	}
	earliest := rows[0].StartedAt
	for _, row := range rows[1:] {
		if earliest == "" || (row.StartedAt != "" && row.StartedAt < earliest) {
			earliest = row.StartedAt
		}
	}
	return earliest
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
    if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -Fqx "OKDEV_JOB_ID=$job_id"; then
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
