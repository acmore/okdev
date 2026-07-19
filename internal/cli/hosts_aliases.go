package cli

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/acmore/okdev/internal/kube"
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

// hostsAliasRewriteScript replaces the managed block in the hosts file.
// /etc/hosts is bind-mounted by the kubelet, so it cannot be renamed over —
// the script filters the previous block and truncate-writes in place. The
// hosts path is a parameter for tests. Busybox-clean: awk, cat, printf.
func hostsAliasRewriteScript(block, hostsPath string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "hosts=%s\n", shellQuote(hostsPath))
	fmt.Fprintf(&b, "keep=$(awk 'BEGIN{skip=0} /^%s/{skip=1; next} /^%s/{skip=0; next} !skip{print}' \"$hosts\")\n", hostsAliasBegin, hostsAliasEnd)
	fmt.Fprintf(&b, "printf '%%s\\n%%s' \"$keep\" %s > \"$hosts\"\n", shellQuote(block))
	return b.String()
}

type hostsAliasClient interface {
	ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error)
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
}

// setupHostAliases provisions the alias block on every running session pod.
// Best-effort per pod: a pod that cannot be written (not running yet,
// non-root image without hosts write access) is reported as a warning, not a
// failure — the next okdev up retries, and single-pod sessions skip the
// whole step (nothing to address).
func setupHostAliases(ctx context.Context, k hostsAliasClient, namespace string, labels map[string]string, container string, warnf func(string, ...any)) (int, error) {
	pods, err := k.ListPods(ctx, namespace, false, workload.DiscoveryLabelSelector(labels))
	if err != nil {
		return 0, fmt.Errorf("list pods for host aliases: %w", err)
	}
	aliases := map[string]string{}
	var running []kube.PodSummary
	names := make([]string, 0, len(pods))
	for _, pod := range pods {
		if pod.Deleting || !strings.EqualFold(pod.Phase, "Running") || strings.TrimSpace(pod.PodIP) == "" {
			continue
		}
		running = append(running, pod)
		names = append(names, pod.Name)
	}
	if len(running) < 2 {
		return 0, nil
	}
	shorts := shortPodNames(names)
	for i, pod := range running {
		aliases[shorts[i]] = pod.PodIP
	}
	script := hostsAliasRewriteScript(buildHostsAliasBlock(aliases), "/etc/hosts")

	var wg sync.WaitGroup
	var mu sync.Mutex
	written := 0
	for _, pod := range running {
		wg.Add(1)
		go func(podName string) {
			defer wg.Done()
			if _, err := k.ExecShInContainer(ctx, namespace, podName, container, script); err != nil {
				warnf("host aliases: %s: %v", podName, err)
				return
			}
			mu.Lock()
			written++
			mu.Unlock()
		}(pod.Name)
	}
	wg.Wait()
	return written, nil
}
