package cli

import (
	"bufio"
	"fmt"
	"io"
	"strings"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/workload"
	"sigs.k8s.io/yaml"
)

type driftKind int

const (
	driftUnknown driftKind = iota
	driftNone
	driftChanged
)

type driftResult struct {
	Kind driftKind
	Diff string
}

func detectDrift(current *config.LastAppliedWorkloadSpec, oldJSON, oldHash string) driftResult {
	if oldHash == "" && oldJSON == "" {
		return driftResult{Kind: driftUnknown}
	}

	currentHash, err := current.SHA256()
	if err != nil {
		return driftResult{Kind: driftUnknown}
	}
	if currentHash == oldHash {
		return driftResult{Kind: driftNone}
	}

	old, err := config.ParseLastAppliedWorkloadSpec(oldJSON)
	if err != nil {
		return driftResult{Kind: driftChanged}
	}

	diff := renderSpecDiff(&old, current)
	return driftResult{Kind: driftChanged, Diff: diff}
}

func renderSpecDiff(old, new *config.LastAppliedWorkloadSpec) string {
	oldYAML, err := yaml.Marshal(old)
	if err != nil {
		return ""
	}
	newYAML, err := yaml.Marshal(new)
	if err != nil {
		return ""
	}
	return unifiedDiff(string(oldYAML), string(newYAML))
}

func unifiedDiff(a, b string) string {
	aLines := splitLines(a)
	bLines := splitLines(b)

	lcs := computeLCS(aLines, bLines)

	var out strings.Builder
	ai, bi, li := 0, 0, 0
	for li < len(lcs) {
		for ai < len(aLines) && aLines[ai] != lcs[li] {
			out.WriteString("- " + aLines[ai] + "\n")
			ai++
		}
		for bi < len(bLines) && bLines[bi] != lcs[li] {
			out.WriteString("+ " + bLines[bi] + "\n")
			bi++
		}
		out.WriteString("  " + lcs[li] + "\n")
		ai++
		bi++
		li++
	}
	for ; ai < len(aLines); ai++ {
		out.WriteString("- " + aLines[ai] + "\n")
	}
	for ; bi < len(bLines); bi++ {
		out.WriteString("+ " + bLines[bi] + "\n")
	}
	return strings.TrimRight(out.String(), "\n")
}

func splitLines(s string) []string {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	if len(lines) == 1 && lines[0] == "" {
		return nil
	}
	return lines
}

func computeLCS(a, b []string) []string {
	m, n := len(a), len(b)
	dp := make([][]int, m+1)
	for i := range dp {
		dp[i] = make([]int, n+1)
	}
	for i := 1; i <= m; i++ {
		for j := 1; j <= n; j++ {
			if a[i-1] == b[j-1] {
				dp[i][j] = dp[i-1][j-1] + 1
			} else if dp[i-1][j] >= dp[i][j-1] {
				dp[i][j] = dp[i-1][j]
			} else {
				dp[i][j] = dp[i][j-1]
			}
		}
	}
	result := make([]string, 0, dp[m][n])
	i, j := m, n
	for i > 0 && j > 0 {
		if a[i-1] == b[j-1] {
			result = append(result, a[i-1])
			i--
			j--
		} else if dp[i-1][j] >= dp[i][j-1] {
			i--
		} else {
			j--
		}
	}
	for l, r := 0, len(result)-1; l < r; l, r = l+1, r-1 {
		result[l], result[r] = result[r], result[l]
	}
	return result
}

type driftAction int

const (
	driftActionReuse driftAction = iota
	driftActionReapply
	driftActionRecreate
)

func handleWorkloadDrift(state *upState) (driftAction, error) {
	refProvider, ok := state.runtime.(workload.RefProvider)
	if !ok {
		return driftActionReuse, nil
	}
	apiVersion, kind, name, err := refProvider.WorkloadRef()
	if err != nil {
		return driftActionReuse, nil
	}

	oldJSON, jsonFound, err := state.command.kube.GetResourceAnnotation(
		state.ctx, state.command.namespace, apiVersion, kind, name,
		config.AnnotationLastAppliedSpec,
	)
	if err != nil {
		return driftActionReuse, nil
	}
	oldHash, hashFound, err := state.command.kube.GetResourceAnnotation(
		state.ctx, state.command.namespace, apiVersion, kind, name,
		config.AnnotationLastAppliedHash,
	)
	if err != nil {
		return driftActionReuse, nil
	}

	if !jsonFound && !hashFound {
		state.ui.warnf("drift detection unavailable for this session (no prior snapshot); will activate after next apply")
		return driftActionReuse, nil
	}

	var currentJSON string
	switch rt := state.runtime.(type) {
	case *workload.PodRuntime:
		currentJSON = rt.LastAppliedSpecJSON
	case *workload.GenericRuntime:
		currentJSON = rt.LastAppliedSpecJSON
	default:
		return driftActionReuse, nil
	}

	currentSnap, err := config.ParseLastAppliedWorkloadSpec(currentJSON)
	if err != nil {
		return driftActionReuse, nil
	}
	result := detectDrift(&currentSnap, oldJSON, oldHash)

	switch result.Kind {
	case driftUnknown, driftNone:
		return driftActionReuse, nil
	case driftChanged:
		isPod := state.runtime.Kind() == workload.TypePod
		return handleChangedWorkloadDrift(state, result.Diff, isPod, isTerminalReader(state.cmd.InOrStdin()))
	}
	return driftActionReuse, nil
}

func handleChangedWorkloadDrift(state *upState, diff string, isPod bool, interactive bool) (driftAction, error) {
	errOut := state.cmd.ErrOrStderr()
	state.ui.stopActive()
	requiresRecreate := isPod || reconcileStrategyForWorkload(state) == workloadApplyRecreated

	if diff != "" {
		fmt.Fprintln(errOut, "Workload spec has changed:")
		fmt.Fprintln(errOut)
		fmt.Fprintln(errOut, diff)
		fmt.Fprintln(errOut)
	} else {
		fmt.Fprintln(errOut, "Workload spec has changed.")
	}

	if requiresRecreate {
		fmt.Fprintln(errOut, "This workload must be recreated to apply these changes.")
	}

	if !interactive {
		action := "apply"
		if requiresRecreate {
			action = "recreate"
		}
		return driftActionReuse, fmt.Errorf("workload spec changed; re-run with --reconcile to %s", action)
	}

	prompt := "Reapply workload? [y/N]: "
	if requiresRecreate {
		prompt = "Recreate workload? [y/N]: "
	}
	ok, promptErr := promptDriftReapply(state.cmd.InOrStdin(), errOut, prompt)
	if promptErr != nil {
		return driftActionReuse, promptErr
	}
	if !ok {
		return driftActionReuse, nil
	}

	if requiresRecreate {
		if state.ui != nil {
			state.ui.stepRun("reconcile", "deleting existing workload")
		}
		if delErr := state.runtime.Delete(state.ctx, state.command.kube, state.command.namespace, true); delErr != nil {
			return driftActionReuse, fmt.Errorf("delete existing workload for recreate: %w", delErr)
		}
		if waitErr := waitForReconcileDeletion(state); waitErr != nil {
			return driftActionReuse, waitErr
		}
		if state.ui != nil {
			state.ui.stepDone("reconcile", "old workload deleted")
		}
		return driftActionRecreate, nil
	}
	return driftActionReapply, nil
}

func promptDriftReapply(in io.Reader, out io.Writer, prompt string) (bool, error) {
	fmt.Fprint(out, prompt)
	reader := bufio.NewReader(in)
	line, err := reader.ReadString('\n')
	if err != nil {
		return false, fmt.Errorf("read confirmation: %w", err)
	}
	answer := strings.TrimSpace(strings.ToLower(line))
	return answer == "y" || answer == "yes", nil
}
