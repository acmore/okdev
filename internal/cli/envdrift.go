package cli

import (
	"context"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strings"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

// installCommandPattern matches package-manager install invocations inside an
// exec command line. Deliberately narrow: only the install-class verbs whose
// effects live in the container overlay and vanish on recreate (#175).
var installCommandPattern = regexp.MustCompile(`\b(apt-get\s+install|apt\s+install|pip3?\s+install|conda\s+install|apk\s+add|dnf\s+install|yum\s+install)\b`)

// installCommandHint returns the one-line reminder when a command installs
// into the container overlay. The change survives only until the next pod
// recreation or in-place container restart — pointing at lifecycle hooks at
// install time is the moment the user still knows which change mattered.
func installCommandHint(commandDisplay string) string {
	if !installCommandPattern.MatchString(commandDisplay) {
		return ""
	}
	return "note: this installs into the container overlay and vanishes on pod recreation; persist it via spec.lifecycle.postCreate/postSync (see `okdev env-diff` to review what has drifted)"
}

const envBaselineDir = "/var/okdev/env-baseline"

// envCollectorScript emits the package inventory of the container: one
// "name=version" line per OS package (dpkg or apk) prefixed with "os:", and
// one "name==version" line per python package prefixed with "py:". Sorted,
// busybox-clean, tolerant of missing package managers.
const envCollectorScript = `{
if command -v dpkg-query >/dev/null 2>&1; then
  dpkg-query -W -f 'os:${Package}=${Version}\n' 2>/dev/null
elif command -v apk >/dev/null 2>&1; then
  apk info -v 2>/dev/null | sed 's/^/os:/'
fi
if command -v pip >/dev/null 2>&1; then
  pip freeze --disable-pip-version-check 2>/dev/null | sed 's/^/py:/'
elif command -v pip3 >/dev/null 2>&1; then
  pip3 freeze --disable-pip-version-check 2>/dev/null | sed 's/^/py:/'
fi
} | sort`

// envBaselineCaptureScript snapshots the inventory to the runtime volume,
// which survives in-place container restarts but dies with the pod — exactly
// the lifetime of the environment it describes a baseline for.
func envBaselineCaptureScript() string {
	return fmt.Sprintf("mkdir -p %s && %s > %s/packages.txt", envBaselineDir, envCollectorScript, envBaselineDir)
}

// envBaselineReadScript prints the stored baseline, failing distinctly when
// none was captured yet.
const envBaselineReadScript = `if [ ! -f ` + envBaselineDir + `/packages.txt ]; then echo 'no baseline captured' >&2; exit 21; fi
cat ` + envBaselineDir + `/packages.txt`

// parsePackageInventory maps package identity (prefix + name) to its full
// inventory line, so version changes surface as "changed" rather than as an
// unrelated add/remove pair.
func parsePackageInventory(raw string) map[string]string {
	out := map[string]string{}
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		name := line
		if i := strings.IndexAny(line, "=<>"); i >= 0 {
			name = line[:i]
		}
		out[name] = line
	}
	return out
}

type envDrift struct {
	Added   []string
	Removed []string
	Changed []string
}

func diffPackageInventories(baseline, current map[string]string) envDrift {
	drift := envDrift{}
	for name, line := range current {
		base, ok := baseline[name]
		if !ok {
			drift.Added = append(drift.Added, line)
			continue
		}
		if base != line {
			drift.Changed = append(drift.Changed, fmt.Sprintf("%s (was %s)", line, base))
		}
	}
	for name, line := range baseline {
		if _, ok := current[name]; !ok {
			drift.Removed = append(drift.Removed, line)
		}
	}
	sort.Strings(drift.Added)
	sort.Strings(drift.Removed)
	sort.Strings(drift.Changed)
	return drift
}

func (d envDrift) empty() bool {
	return len(d.Added) == 0 && len(d.Removed) == 0 && len(d.Changed) == 0
}

// postCreateDraft renders ready-to-paste lifecycle hook lines that would
// reproduce the added packages on a fresh pod.
func (d envDrift) postCreateDraft() string {
	var aptPkgs, pipPkgs []string
	for _, line := range d.Added {
		switch {
		case strings.HasPrefix(line, "os:"):
			aptPkgs = append(aptPkgs, strings.TrimPrefix(line, "os:"))
		case strings.HasPrefix(line, "py:"):
			pipPkgs = append(pipPkgs, strings.TrimPrefix(line, "py:"))
		}
	}
	var parts []string
	if len(aptPkgs) > 0 {
		parts = append(parts, "apt-get update && apt-get install -y "+strings.Join(aptPkgs, " "))
	}
	if len(pipPkgs) > 0 {
		parts = append(parts, "pip install "+strings.Join(pipPkgs, " "))
	}
	if len(parts) == 0 {
		return ""
	}
	return "spec:\n  lifecycle:\n    postCreate: |\n      " + strings.Join(parts, " && \\\n      ")
}

func printEnvDrift(out io.Writer, pod string, drift envDrift) {
	if drift.empty() {
		fmt.Fprintf(out, "%s: no package drift since the baseline (manual installs would be listed here)\n", pod)
		return
	}
	fmt.Fprintf(out, "%s: package drift since the post-hook baseline:\n", pod)
	for _, line := range drift.Added {
		fmt.Fprintf(out, "  + %s\n", line)
	}
	for _, line := range drift.Changed {
		fmt.Fprintf(out, "  ~ %s\n", line)
	}
	for _, line := range drift.Removed {
		fmt.Fprintf(out, "  - %s\n", line)
	}
	if draft := drift.postCreateDraft(); draft != "" {
		fmt.Fprintf(out, "\nTo survive pod recreation, add the additions to your config:\n%s\n", draft)
	}
}

type envDriftClient interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

func newEnvDiffCmd(opts *Options) *cobra.Command {
	var podNames []string
	var container string
	cmd := &cobra.Command{
		Use:   "env-diff [session]",
		Short: "Show packages installed by hand since the session came up",
		Long: `Diffs the pod's current package inventory (dpkg/apk + pip) against the
baseline captured after lifecycle hooks ran, so debugging-time installs that
would silently vanish on the next pod recreation become visible — with a
ready-to-paste postCreate draft to persist them.`,
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
			pods, err := selectSessionPods(cmd.Context(), cc, podNames, "", nil, nil, true)
			if err != nil {
				return err
			}
			targetContainer := strings.TrimSpace(container)
			if targetContainer == "" {
				targetContainer = resolveTargetContainer(cc.cfg)
			}
			if len(podNames) == 0 {
				// Default to the target pod: drift comes from interactive
				// debugging, which happens there.
				target, err := resolveTargetRef(cmd.Context(), cc.opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
				if err != nil {
					return err
				}
				filtered := pods[:0]
				for _, pod := range pods {
					if pod.Name == target.PodName {
						filtered = append(filtered, pod)
					}
				}
				pods = filtered
			}
			for _, pod := range pods {
				if err := runEnvDiffForPod(cmd.Context(), cc.kube, cc.namespace, pod.Name, targetContainer, cmd.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		},
	}
	cmd.Flags().StringSliceVar(&podNames, "pod", nil, "Diff specific pods (default: the target pod)")
	cmd.Flags().StringVar(&container, "container", "", "Override target container")
	return cmd
}

func runEnvDiffForPod(ctx context.Context, k envDriftClient, namespace, pod, container string, out io.Writer) error {
	baselineRaw, err := k.ExecShInContainer(ctx, namespace, pod, container, envBaselineReadScript)
	if err != nil {
		return fmt.Errorf("%s: read env baseline (captured by okdev up after hooks): %w — run `okdev up` once to capture it", pod, err)
	}
	currentRaw, err := k.ExecShInContainer(ctx, namespace, pod, container, envCollectorScript)
	if err != nil {
		return fmt.Errorf("%s: collect current package inventory: %w", pod, err)
	}
	drift := diffPackageInventories(parsePackageInventory(string(baselineRaw)), parsePackageInventory(string(currentRaw)))
	printEnvDrift(out, pod, drift)
	return nil
}

type envBaselineClient interface {
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

// captureEnvBaselines snapshots each running pod's package inventory after
// lifecycle hooks. force overwrites (hooks just ran, so hook-installed
// packages belong in the baseline); otherwise only missing baselines are
// written, so a resumed `okdev up` cannot swallow manual drift into a fresh
// baseline. Best-effort per pod.
func captureEnvBaselines(ctx context.Context, k envBaselineClient, namespace string, labels map[string]string, container string, force bool, warnf func(string, ...any)) int {
	pods, err := k.ListPods(ctx, namespace, false, workload.DiscoveryLabelSelector(labels))
	if err != nil {
		warnf("env baseline: list pods: %v", err)
		return 0
	}
	script := envBaselineCaptureScript()
	if !force {
		script = fmt.Sprintf("[ -f %s/packages.txt ] || { %s; }", envBaselineDir, script)
	}
	captured := 0
	for _, pod := range pods {
		if pod.Deleting || !strings.EqualFold(pod.Phase, "Running") {
			continue
		}
		if _, err := k.ExecShInContainer(ctx, namespace, pod.Name, container, script); err != nil {
			warnf("env baseline: %s: %v", pod.Name, err)
			continue
		}
		captured++
	}
	return captured
}
