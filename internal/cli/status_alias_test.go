package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestDetailedStatusPrintsPodAlias(t *testing.T) {
	var sb strings.Builder
	printDetailedStatus(&sb, detailedStatus{
		Session: "sess",
		Pods: []detailedStatusPod{
			{Name: "okdev-sess-abc-master-0", Alias: "master-0", Phase: "Running", Ready: "1/1"},
			{Name: "okdev-sess-abc-worker-1", Alias: "worker-1", Phase: "Running", Ready: "1/1"},
		},
	})
	got := sb.String()
	// The docs told readers to learn short names here; nothing printed them,
	// so the only working way was to mistype --pod and read the error (#223).
	for _, want := range []string{"alias=master-0", "alias=worker-1"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %q in:\n%s", want, got)
		}
	}
}

func TestDetailedStatusOmitsRedundantAlias(t *testing.T) {
	// A single-pod session has no common prefix to strip, so the alias is the
	// pod name — printing it twice is noise.
	var sb strings.Builder
	printDetailedStatus(&sb, detailedStatus{
		Session: "sess",
		Pods:    []detailedStatusPod{{Name: "okdev-sess-abc", Alias: "okdev-sess-abc", Phase: "Running"}},
	})
	if strings.Contains(sb.String(), "alias=") {
		t.Fatalf("alias identical to the pod name must not be repeated:\n%s", sb.String())
	}
}

func TestShortPodNamesMatchWhatPodSelectorsAccept(t *testing.T) {
	// The ALIAS column and --pod resolution must speak the same vocabulary,
	// which is the whole point of listing it (#223).
	names := []string{"okdev-sess-abc-master-0", "okdev-sess-abc-worker-0", "okdev-sess-abc-worker-1"}
	aliases := shortPodNames(names)
	want := []string{"master-0", "worker-0", "worker-1"}
	for i := range want {
		if aliases[i] != want[i] {
			t.Fatalf("alias[%d] = %q, want %q", i, aliases[i], want[i])
		}
	}
	summaries := make([]kube.PodSummary, 0, len(names))
	for _, name := range names {
		summaries = append(summaries, kube.PodSummary{Name: name})
	}
	for _, alias := range aliases {
		resolved, err := resolvePodAliases(summaries, []string{alias})
		if err != nil || len(resolved) != 1 {
			t.Fatalf("--pod %q must resolve to exactly the pod the column names: %v", alias, err)
		}
	}
}
