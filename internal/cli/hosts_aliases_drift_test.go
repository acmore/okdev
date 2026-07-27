package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func aliasRecord(entries ...session.HostAliasEntry) session.HostAliases {
	return session.HostAliases{At: time.Now().UTC().Add(-time.Hour), Entries: entries}
}

func TestHostAliasesStale(t *testing.T) {
	livePods := []kube.PodSummary{
		{Name: "okdev-s-abc-master-0", UID: "uid-m", Phase: "Running", PodIP: "10.244.0.5"},
		{Name: "okdev-s-abc-worker-0", UID: "uid-w", Phase: "Running", PodIP: "10.244.0.6"},
	}
	current := aliasRecord(
		session.HostAliasEntry{Alias: "master-0", Pod: "okdev-s-abc-master-0", UID: "uid-m", IP: "10.244.0.5"},
		session.HostAliasEntry{Alias: "worker-0", Pod: "okdev-s-abc-worker-0", UID: "uid-w", IP: "10.244.0.6"},
	)

	if reason := hostAliasesStale(current, livePods); reason != "" {
		t.Fatalf("an unchanged session must not report drift, got %q", reason)
	}

	// The case the feature exists for: a controller recreated the pod, which
	// keeps its NAME and gets a new UID and IP. A name-keyed comparison would
	// see nothing wrong here, which is why UID is recorded.
	recreated := []kube.PodSummary{
		livePods[0],
		{Name: "okdev-s-abc-worker-0", UID: "uid-w2", Phase: "Running", PodIP: "10.244.0.9"},
	}
	reason := hostAliasesStale(current, recreated)
	if !strings.Contains(reason, "okdev-s-abc-worker-0") || !strings.Contains(reason, "recreated") {
		t.Fatalf("expected a recreation reason, got %q", reason)
	}

	// A pod that joined after the last write has a virgin /etc/hosts.
	joined := append(append([]kube.PodSummary{}, livePods...),
		kube.PodSummary{Name: "okdev-s-abc-worker-1", UID: "uid-w1", Phase: "Running", PodIP: "10.244.0.7"})
	if reason := hostAliasesStale(current, joined); !strings.Contains(reason, "worker-1") {
		t.Fatalf("expected the new pod to be reported, got %q", reason)
	}

	// A pod that is gone leaves peers mapping a dead address.
	if reason := hostAliasesStale(current, livePods[:1]); reason != "" {
		t.Fatalf("a single remaining pod has nothing to address, got %q", reason)
	}
	shrunk := []kube.PodSummary{
		livePods[0],
		{Name: "okdev-s-abc-worker-2", UID: "uid-w2", Phase: "Running", PodIP: "10.244.0.8"},
	}
	if reason := hostAliasesStale(current, shrunk); reason == "" {
		t.Fatal("expected drift when the recorded pod is gone")
	}

	// Never written, but the session now has peers: missing, not merely stale.
	if reason := hostAliasesStale(session.HostAliases{}, livePods); !strings.Contains(reason, "no alias block") {
		t.Fatalf("expected the never-written reason, got %q", reason)
	}
	// Single-pod sessions have nothing to address either way.
	if reason := hostAliasesStale(session.HostAliases{}, livePods[:1]); reason != "" {
		t.Fatalf("single-pod session must not report drift, got %q", reason)
	}
}

func TestHostsAliasRewriteScriptFailsOnReadbackMismatch(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	if err := os.WriteFile(hosts, []byte("127.0.0.1 localhost\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Claim three entries while writing two: the readback must reject it.
	// Before #220 the only check was that the exec returned no error, so a
	// write that landed nowhere looked exactly like one that worked.
	block := buildHostsAliasBlock(map[string]string{"master-0": "10.244.0.5", "worker-0": "10.244.0.6"})
	out, err := runHostsScriptErr(t, hostsAliasRewriteScript(block, hosts, 3))
	if err == nil {
		t.Fatalf("expected a non-zero exit on readback mismatch, output: %s", out)
	}
	if !strings.Contains(out, "readback mismatch") {
		t.Fatalf("expected the mismatch to be named, got %q", out)
	}

	// The honest count passes.
	if out, err := runHostsScriptErr(t, hostsAliasRewriteScript(block, hosts, 2)); err != nil {
		t.Fatalf("matching readback must succeed: %v (%s)", err, out)
	}
}

func TestPlanHostAliasesNamesSkippedPods(t *testing.T) {
	plan := planHostAliases([]kube.PodSummary{
		{Name: "m-0", Phase: "Running", PodIP: "10.0.0.1"},
		{Name: "w-0", Phase: "Running", PodIP: "10.0.0.2"},
		{Name: "w-1", Phase: "Pending"},
		{Name: "w-2", Phase: "Running", PodIP: "10.0.0.3", Deleting: true},
		{Name: "w-3", Phase: "Running"}, // running but no IP assigned yet
	})
	if len(plan.Running) != 2 || len(plan.Aliases) != 2 {
		t.Fatalf("unexpected plan %+v", plan)
	}
	joined := strings.Join(plan.Skipped, " | ")
	for _, want := range []string{"w-1 (Pending)", "w-2 (terminating)", "w-3 (no pod IP yet)"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("expected %q among skipped, got %q", want, joined)
		}
	}
}
