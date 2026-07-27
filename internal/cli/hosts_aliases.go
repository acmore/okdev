package cli

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
)

const (
	hostsAliasBegin = "# okdev-aliases-begin"
	hostsAliasEnd   = "# okdev-aliases-end"
)

// buildHostsAliasBlock renders the managed /etc/hosts block mapping every
// session pod's short alias (master-0, worker-1) to its current IP, so
// launch scripts can hardcode MASTER_ADDR=master-0 once and survive pod
// recreations (#169) — okdev rewrites the block on every up/restart, the
// same refresh lifecycle as inter-pod SSH config and lifecycle hooks.
func buildHostsAliasBlock(aliases map[string]string) string {
	if len(aliases) == 0 {
		return ""
	}
	names := make([]string, 0, len(aliases))
	for name := range aliases {
		names = append(names, name)
	}
	sort.Strings(names)
	var b strings.Builder
	b.WriteString(hostsAliasBegin + " (managed by okdev; do not edit)\n")
	for _, name := range names {
		fmt.Fprintf(&b, "%s %s\n", aliases[name], name)
	}
	b.WriteString(hostsAliasEnd + "\n")
	return b.String()
}

// hostsAliasRewriteScript replaces the managed block in the hosts file and
// reads it back. /etc/hosts is bind-mounted by the kubelet, so it cannot be
// renamed over — the script filters the previous block and truncate-writes in
// place. The hosts path is a parameter for tests. Busybox-clean: awk, printf.
//
// The readback is the point of the trailing lines (#220): a write that landed
// nowhere used to be indistinguishable from one that worked, because the only
// check was that the exec itself returned no error. Counting the alias lines
// back out of the file turns a silent no-op into a non-zero exit that the
// caller already reports. It counts rather than resolves because `getent` is
// not present in every image, and the file is what okdev actually controls.
func hostsAliasRewriteScript(block, hostsPath string, wantEntries int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hosts=%s\n", shellQuote(hostsPath))
	fmt.Fprintf(&b, "keep=$(awk 'BEGIN{skip=0} /^%s/{skip=1; next} /^%s/{skip=0; next} !skip{print}' \"$hosts\")\n", hostsAliasBegin, hostsAliasEnd)
	fmt.Fprintf(&b, "printf '%%s\\n%%s' \"$keep\" %s > \"$hosts\"\n", shellQuote(block))
	fmt.Fprintf(&b, "got=$(awk 'BEGIN{c=0;s=0} /^%s/{s=1; next} /^%s/{s=0; next} s&&NF{c++} END{print c}' \"$hosts\")\n", hostsAliasBegin, hostsAliasEnd)
	fmt.Fprintf(&b, "[ \"$got\" = %d ] || { echo \"okdev: host alias readback mismatch ($got of %d entries)\" >&2; exit 3; }\n", wantEntries, wantEntries)
	return b.String()
}

// hostAliasPlan is the alias map okdev would write for a set of pods, plus the
// pods it cannot write to and why. The skipped list is reported rather than
// counted (#220): a partially written session used to print the same "written
// on N pod(s)" line as a complete one, so a pod that never received the block
// — and in which `master-0` therefore does not resolve — was invisible.
type hostAliasPlan struct {
	Running []kube.PodSummary
	Aliases map[string]string
	Skipped []string
}

func planHostAliases(pods []kube.PodSummary) hostAliasPlan {
	plan := hostAliasPlan{Aliases: map[string]string{}}
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		switch {
		case pod.Deleting:
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s (terminating)", pod.Name))
		case !strings.EqualFold(pod.Phase, "Running"):
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s (%s)", pod.Name, pod.Phase))
		case strings.TrimSpace(pod.PodIP) == "":
			plan.Skipped = append(plan.Skipped, fmt.Sprintf("%s (no pod IP yet)", pod.Name))
		default:
			plan.Running = append(plan.Running, pod)
			names = append(names, pod.Name)
		}
	}
	shorts := shortPodNames(names)
	for i, pod := range plan.Running {
		plan.Aliases[shorts[i]] = pod.PodIP
	}
	return plan
}

// hostAliasesStale compares the live pods against the block okdev last wrote.
// A controller-driven recreation (OOM kill, eviction, node drain, operator
// restart) keeps the pod name and changes the UID and IP, so the recreated pod
// has a virgin /etc/hosts while every surviving peer still maps the dead pod's
// address — neither of which fails loudly (#220). Returns the human reason for
// the first difference found, or "" when the block is still accurate.
func hostAliasesStale(record session.HostAliases, pods []kube.PodSummary) string {
	plan := planHostAliases(pods)
	if len(plan.Running) < 2 {
		return ""
	}
	if record.At.IsZero() || len(record.Entries) == 0 {
		// Nothing was ever written for a session that now has peers to
		// address — the aliases are missing, not merely stale.
		return "no alias block has been written for this session"
	}
	byPod := make(map[string]session.HostAliasEntry, len(record.Entries))
	for _, entry := range record.Entries {
		byPod[entry.Pod] = entry
	}
	for alias, ip := range plan.Aliases {
		var live kube.PodSummary
		for _, pod := range plan.Running {
			if pod.PodIP == ip {
				live = pod
				break
			}
		}
		entry, ok := byPod[live.Name]
		if !ok {
			return fmt.Sprintf("%s (%s) joined the session after the last write", alias, live.Name)
		}
		if entry.UID != "" && live.UID != "" && entry.UID != live.UID {
			return fmt.Sprintf("%s was recreated since the last write (alias still points at %s)", live.Name, entry.IP)
		}
		if entry.IP != ip {
			return fmt.Sprintf("%s moved from %s to %s since the last write", alias, entry.IP, ip)
		}
	}
	liveNames := make(map[string]bool, len(plan.Running))
	for _, pod := range plan.Running {
		liveNames[pod.Name] = true
	}
	for _, entry := range record.Entries {
		if !liveNames[entry.Pod] {
			return fmt.Sprintf("%s (%s) is gone but peers still map its address", entry.Alias, entry.Pod)
		}
	}
	return ""
}

// hostAliasRefreshHint is the one-line cue for a stale block okdev is not
// going to fix on this code path.
func hostAliasRefreshHint(reason string) string {
	return fmt.Sprintf("warning: host aliases are stale — %s; short names like master-0 resolve to the wrong address until `okdev up` rewrites them", reason)
}

type hostsAliasClient interface {
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

// hostAliasWriteResult reports what a write actually achieved, so the caller
// can say "written on 2 of 3 pods, worker-1 pending" instead of a bare count.
type hostAliasWriteResult struct {
	Written int
	Skipped []string
	Failed  []string
}

// Complete reports whether every pod okdev knows about carries the block.
func (r hostAliasWriteResult) Complete() bool {
	return len(r.Skipped) == 0 && len(r.Failed) == 0
}

// Summary renders the result for the up step line.
func (r hostAliasWriteResult) Summary() string {
	out := fmt.Sprintf("short-name aliases written on %d pod(s)", r.Written)
	if len(r.Skipped) > 0 {
		out += fmt.Sprintf("; skipped %s", strings.Join(r.Skipped, ", "))
	}
	if len(r.Failed) > 0 {
		out += fmt.Sprintf("; failed on %s", strings.Join(r.Failed, ", "))
	}
	if !r.Complete() {
		out += " (short names do not resolve in those pods until the next `okdev up`)"
	}
	return out
}

// setupHostAliases provisions the alias block on every running session pod.
// Best-effort per pod: a pod that cannot be written (not running yet,
// non-root image without hosts write access) is reported as a warning, not a
// failure — the next okdev up retries, and single-pod sessions skip the
// whole step (nothing to address).
func setupHostAliases(ctx context.Context, k hostsAliasClient, namespace, sessionName string, labels map[string]string, container string, warnf func(string, ...any)) (hostAliasWriteResult, error) {
	pods, err := k.ListPods(ctx, namespace, false, workload.DiscoveryLabelSelector(labels))
	if err != nil {
		return hostAliasWriteResult{}, fmt.Errorf("list pods for host aliases: %w", err)
	}
	return applyHostAliases(ctx, k, namespace, sessionName, pods, container, warnf)
}

// applyHostAliases writes the block for an already-listed set of pods. Split
// out from setupHostAliases so callers that have just listed the session's
// pods — the exec path, which self-heals a stale block — do not pay for a
// second listing (#220).
func applyHostAliases(ctx context.Context, k hostsAliasClient, namespace, sessionName string, pods []kube.PodSummary, container string, warnf func(string, ...any)) (hostAliasWriteResult, error) {
	plan := planHostAliases(pods)
	if len(plan.Running) < 2 {
		return hostAliasWriteResult{}, nil
	}
	script := hostsAliasRewriteScript(buildHostsAliasBlock(plan.Aliases), "/etc/hosts", len(plan.Aliases))

	var wg sync.WaitGroup
	var mu sync.Mutex
	result := hostAliasWriteResult{Skipped: plan.Skipped}
	byIP := make(map[string]kube.PodSummary, len(plan.Running))
	for _, pod := range plan.Running {
		byIP[pod.PodIP] = pod
	}
	for _, pod := range plan.Running {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			if _, err := k.ExecShInContainer(ctx, namespace, podName, container, script); err != nil {
				warnf("host aliases: %s: %v", podName, err)
				mu.Lock()
				result.Failed = append(result.Failed, podName)
				mu.Unlock()
				return
			}
			mu.Lock()
			result.Written++
			mu.Unlock()
		}(pod.Name)
	}
	wg.Wait()
	sort.Strings(result.Failed)

	// Record only what every pod agrees on. A block that reached some pods is
	// not a state okdev can later compare against, so a partial write stays
	// detectable as stale rather than being cached as the truth.
	if result.Complete() {
		record := session.HostAliases{At: time.Now().UTC()}
		aliasNames := make([]string, 0, len(plan.Aliases))
		for alias := range plan.Aliases {
			aliasNames = append(aliasNames, alias)
		}
		sort.Strings(aliasNames)
		for _, alias := range aliasNames {
			ip := plan.Aliases[alias]
			record.Entries = append(record.Entries, session.HostAliasEntry{
				Alias: alias,
				Pod:   byIP[ip].Name,
				UID:   byIP[ip].UID,
				IP:    ip,
			})
		}
		if err := session.SaveHostAliases(sessionName, record); err != nil {
			slog.Debug("failed to record host aliases", "session", sessionName, "error", err)
		}
	}
	return result, nil
}

// targetContainerForAliases resolves which container the alias write runs in,
// honouring an explicit --container override.
func targetContainerForAliases(cc *commandContext, container string) string {
	if strings.TrimSpace(container) != "" {
		return container
	}
	if cc == nil {
		return ""
	}
	return resolveTargetContainer(cc.cfg)
}

// refreshStaleHostAliases rewrites the alias block when the pods no longer
// match what okdev last wrote. Best-effort and silent when nothing drifted:
// the common case must cost nothing, and a session whose pods are unchanged
// is the common case.
//
// Repairing here rather than only reporting is deliberate. `okdev up` already
// refreshes the block, but the gap this closes is precisely the one where no
// okdev command ran between the recreation and the launch — telling the caller
// to run `up` first would leave the silent-hang failure in place for anyone
// who does not read stderr before their command executes.
func refreshStaleHostAliases(ctx context.Context, cc *commandContext, pods []kube.PodSummary, container string, warnOut io.Writer) {
	if cc == nil || cc.kube == nil || strings.TrimSpace(cc.sessionName) == "" {
		return
	}
	record, err := session.LoadHostAliases(cc.sessionName)
	if err != nil {
		slog.Debug("failed to load host alias record", "session", cc.sessionName, "error", err)
		return
	}
	reason := hostAliasesStale(record, pods)
	if reason == "" {
		return
	}
	fmt.Fprintf(warnOut, "notice: refreshing stale host aliases — %s\n", reason)
	result, err := applyHostAliases(ctx, cc.kube, cc.namespace, cc.sessionName, pods, container,
		func(format string, args ...any) { fmt.Fprintf(warnOut, "warning: "+format+"\n", args...) })
	if err != nil {
		fmt.Fprintln(warnOut, hostAliasRefreshHint(reason))
		return
	}
	if !result.Complete() {
		fmt.Fprintf(warnOut, "warning: host aliases: %s\n", result.Summary())
	}
}
