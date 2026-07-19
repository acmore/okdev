package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sort"
	"strconv"
	"strings"
	"time"

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
	PGID      int    `json:"pgid,omitempty"`
	GroupLive int    `json:"groupLive,omitempty"`
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
			commandLimit := jobsListCommandLimit(out, jobs)
			table := make([][]string, 0, len(jobs))
			for _, job := range jobs {
				table = append(table, []string{
					job.JobID,
					job.SummaryState,
					fmt.Sprintf("%d", job.Pods),
					job.StartedAt,
					truncateTableCell(flattenCommandForTable(job.Command), commandLimit),
				})
			}
			output.PrintTable(out, []string{"JOB ID", "STATE", "PODS", "STARTED", "COMMAND"}, table)
			if running := firstRunningJobID(jobs); running != "" {
				fmt.Fprintf(out, "\nhint: `okdev jobs wait %s` blocks until it finishes; add --grep PATTERN to return when the log matches\n", running)
			}
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
			if err != nil && container != syncthingContainerName && isContainerUnavailableExecError(err) {
				// The target container is gone (OOMKilled, crashed). Job
				// metadata lives on the shared /var/okdev volume, so the
				// always-running sidecar can still read it.
				stdout.Reset()
				if sideErr := connect.RunOnContainer(ctx, client, namespace, pod.Name, syncthingContainerName, detachJobsCommand(), false, nil, &stdout, io.Discard); sideErr == nil {
					err = nil
				}
			}
			if err != nil {
				if isContainerUnavailableExecError(err) {
					if hint := containerUnavailableHint(ctx, client, namespace, pod.Name, container); hint != "" {
						err = fmt.Errorf("%w; %s", err, hint)
					}
				}
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
			PGID:      job.PGID,
			GroupLive: job.GroupLive,
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
		if failed > 0 {
			// Some members died while others still run — the stranded-rank
			// shape (#179): surviving ranks likely hang in rendezvous/NCCL
			// holding GPU memory.
			return fmt.Sprintf("degraded(%d running, %d failed of %d)", running, failed, total)
		}
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

// jobsListCommandLimit bounds the COMMAND column so long training commands
// don't wrap table rows on a terminal. Returns 0 (no limit) for non-TTY
// output — piped consumers get the full command; --json is the structured
// path either way.
func jobsListCommandLimit(out io.Writer, jobs []logicalExecJobView) int {
	if !isTerminalFD(out) {
		return 0
	}
	return jobsListCommandLimitForWidth(terminalWidth(), jobs)
}

func jobsListCommandLimitForWidth(termWidth int, jobs []logicalExecJobView) int {
	if termWidth <= 0 {
		return 0
	}
	widths := []int{len("JOB ID"), len("STATE"), len("PODS"), len("STARTED")}
	for _, job := range jobs {
		for i, s := range []string{job.JobID, job.SummaryState, fmt.Sprintf("%d", job.Pods), job.StartedAt} {
			if len(s) > widths[i] {
				widths[i] = len(s)
			}
		}
	}
	fixed := 0
	for _, w := range widths {
		fixed += w
	}
	// PrintTable joins columns with two spaces; four separators precede the
	// COMMAND column.
	limit := termWidth - fixed - 4*2
	// Keep enough of the command to recognize the job even on narrow
	// terminals; a slightly wrapped row beats an unreadable stub.
	if limit < 16 {
		limit = 16
	}
	return limit
}

// flattenCommandForTable renders a possibly multi-line command as a single
// table row: shell line-continuations (`\` + newline) are joined and all
// remaining whitespace runs collapse to single spaces. Embedded newlines
// would otherwise break the one-row-per-job layout no matter how the length
// is capped. JSON output keeps the raw command.
func flattenCommandForTable(s string) string {
	s = strings.ReplaceAll(s, "\\\r\n", " ")
	s = strings.ReplaceAll(s, "\\\n", " ")
	return strings.Join(strings.Fields(s), " ")
}

func truncateTableCell(s string, limit int) string {
	if limit <= 0 {
		return s
	}
	r := []rune(s)
	if len(r) <= limit {
		return s
	}
	return string(r[:limit-1]) + "…"
}

func detachJobsCommand() []string {
	// Each output line is "<alive>\t<groupLive>\t<json>". <alive> is 1 if the
	// job's pid is still running and its /proc/<pid>/environ contains the
	// OKDEV_JOB_ID we set in the wrapper. Matching on an environment
	// variable (rather than cmdline) is robust against PID reuse even when
	// the job's cmdline is the arbitrary user command. <groupLive> counts
	// live (non-zombie) processes still in the job's process group — the
	// leader may be gone while its children (e.g. torchrun workers) linger,
	// which is exactly what `jobs stop` must clean up. The group is
	// identity-checked via OKDEV_JOB_ID in a member's environ (inherited by
	// all descendants) before counting: a PGID number can only be recycled
	// once the whole group is dead, so a mismatch means an unrelated group.
	// The find(1) call also reaps stale .tmp files left behind by killed
	// launchers/wrappers. Both the current metadata dir and the legacy /tmp
	// location are scanned so jobs launched by an older okdev remain
	// visible after an upgrade.
	script := `for dir in '` + detachMetadataDir + `' '` + legacyDetachMetadataDir + `'; do
  [ -d "$dir" ] || continue
  find "$dir" -maxdepth 1 -name '*.tmp' -type f -mmin +1 -delete 2>/dev/null || true
  for f in "$dir"/*.json; do
    [ -e "$f" ] || continue
    contents=$(cat "$f")
    pid=$(printf '%s' "$contents" | sed -n 's/.*"pid":\([0-9][0-9]*\).*/\1/p' | head -n1)
    pgid=$(printf '%s' "$contents" | sed -n 's/.*"pgid":\([0-9][0-9]*\).*/\1/p' | head -n1)
    job_id=$(basename "$f" .json)
    alive=0
    if [ -n "$pid" ] && [ -r "/proc/$pid/environ" ]; then
      if tr '\0' '\n' < "/proc/$pid/environ" 2>/dev/null | grep -Fqx "OKDEV_JOB_ID=$job_id"; then
        alive=1
      fi
    fi
    glive=0
    if [ -n "$pgid" ] && [ "$pgid" -gt 0 ] 2>/dev/null; then
      members=$(awk -v g="$pgid" '{ pid=$1; line=$0; sub(/^[0-9]+ \(.*\) /, "", line); split(line, f, " "); if (f[1] != "Z" && f[3] == g) print pid }' /proc/[0-9]*/stat 2>/dev/null)
      for m in $members; do
        if tr '\0' '\n' < "/proc/$m/environ" 2>/dev/null | grep -Fqx "OKDEV_JOB_ID=$job_id"; then
          glive=$(set -- $members; echo $#)
          break
        fi
      done
    fi
    printf '%s\t%s\t%s\n' "$alive" "$glive" "$contents"
  done
done
`
	return []string{"sh", "-c", script}
}

// isContainerUnavailableExecError reports whether an exec failure means the
// container itself is gone or not running (kubelet/CRI phrasing varies), as
// opposed to the command failing. Only then is the sidecar fallback useful.
func isContainerUnavailableExecError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "container not found") ||
		strings.Contains(msg, "is not running") ||
		strings.Contains(msg, "not created or running") ||
		strings.Contains(msg, "stopped container") ||
		strings.Contains(msg, "container_exited")
}

// containerLastStateClient is satisfied by *kube.Client; kept as a narrow
// interface so exec paths that only hold a connect.ExecClient can probe for
// the capability.
type containerLastStateClient interface {
	GetContainerRestartInfo(ctx context.Context, namespace, pod, container string) (*kube.ContainerRestartInfo, error)
}

// containerUnavailableHint explains why an exec target container is gone,
// e.g. `container "pytorch" terminated: OOMKilled (exit 137, finished
// 2026-07-05T06:32:11Z)`. Returns "" when the cause cannot be determined —
// callers append it to the raw exec error, never replace it.
func containerUnavailableHint(ctx context.Context, client any, namespace, pod, container string) string {
	if strings.TrimSpace(container) == "" {
		return ""
	}
	g, ok := client.(containerLastStateClient)
	if !ok {
		return ""
	}
	hintCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ri, err := g.GetContainerRestartInfo(hintCtx, namespace, pod, container)
	if err != nil || ri == nil || ri.LastReason == "" {
		return ""
	}
	hint := fmt.Sprintf("container %q terminated: %s (exit %d", container, ri.LastReason, ri.LastExitCode)
	if !ri.LastFinishedAt.IsZero() {
		hint += ", finished " + ri.LastFinishedAt.UTC().Format(time.RFC3339)
	}
	return hint + ")"
}

func parseDetachMetadataLines(raw string) ([]detachMetadata, error) {
	lines := strings.Split(raw, "\n")
	out := make([]detachMetadata, 0)
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		// Lines from detachJobsCommand are "<alive>\t<groupLive>\t<json>";
		// tolerate the older "<alive>\t<json>" shape and raw JSON so mixed
		// cli versions against the same pod keep working.
		alive := true
		groupLive := 0
		if tab := strings.IndexByte(line, '\t'); tab >= 0 {
			alive = line[:tab] != "0"
			line = line[tab+1:]
			if tab2 := strings.IndexByte(line, '\t'); tab2 >= 0 {
				if n, err := strconv.Atoi(strings.TrimSpace(line[:tab2])); err == nil && n >= 0 {
					groupLive = n
					line = line[tab2+1:]
				}
			}
		}
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		var job detachMetadata
		if err := json.Unmarshal([]byte(line), &job); err != nil {
			return nil, fmt.Errorf("parse detach job metadata: %w", err)
		}
		job.GroupLive = groupLive
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

// firstRunningJobID returns a running job's id for the jobs-list hint —
// `jobs wait` went undiscovered for whole sessions of hand-rolled sleep
// polling (#191), so the listing points at it whenever waiting is possible.
func firstRunningJobID(jobs []logicalExecJobView) string {
	for _, job := range jobs {
		for _, row := range job.PodStates {
			if row.State == "running" {
				return job.JobID
			}
		}
	}
	return ""
}
