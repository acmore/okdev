package cli

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func runHostsScript(t *testing.T, script string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", script)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("hosts script: %v (%s)", err, out)
	}
}

func TestHostsAliasRewriteScriptIsIdempotentAndPreservesContent(t *testing.T) {
	dir := t.TempDir()
	hosts := filepath.Join(dir, "hosts")
	base := "127.0.0.1 localhost\n10.0.0.1 cluster.local\n"
	if err := os.WriteFile(hosts, []byte(base), 0o644); err != nil {
		t.Fatal(err)
	}

	block1 := buildHostsAliasBlock(map[string]string{"master-0": "10.244.0.5", "worker-0": "10.244.0.6"})
	runHostsScript(t, hostsAliasRewriteScript(block1, hosts))
	got, _ := os.ReadFile(hosts)
	for _, want := range []string{"127.0.0.1 localhost", "10.244.0.5 master-0", "10.244.0.6 worker-0"} {
		if !strings.Contains(string(got), want) {
			t.Fatalf("missing %q in hosts:\n%s", want, got)
		}
	}

	// Second write with new IPs replaces the managed block, not appends.
	block2 := buildHostsAliasBlock(map[string]string{"master-0": "10.244.0.9", "worker-0": "10.244.0.6"})
	runHostsScript(t, hostsAliasRewriteScript(block2, hosts))
	got, _ = os.ReadFile(hosts)
	if strings.Contains(string(got), "10.244.0.5") {
		t.Fatalf("old IP must be replaced:\n%s", got)
	}
	if strings.Count(string(got), hostsAliasBegin) != 1 {
		t.Fatalf("managed block must not duplicate:\n%s", got)
	}
	if !strings.Contains(string(got), "127.0.0.1 localhost") {
		t.Fatalf("kubelet-managed content must be preserved:\n%s", got)
	}
}

type fakeHostsClient struct {
	mu      sync.Mutex
	pods    []kube.PodSummary
	scripts map[string]string
}

func (f *fakeHostsClient) ListPods(context.Context, string, bool, string) ([]kube.PodSummary, error) {
	return f.pods, nil
}

func (f *fakeHostsClient) ExecShInContainer(_ context.Context, _ string, pod, _ string, script string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.scripts == nil {
		f.scripts = map[string]string{}
	}
	f.scripts[pod] = script
	return nil, nil
}

func TestSetupHostAliasesWritesAllRunningPods(t *testing.T) {
	client := &fakeHostsClient{pods: []kube.PodSummary{
		{Name: "okdev-s-abc-master-0", Phase: "Running", PodIP: "10.244.0.5"},
		{Name: "okdev-s-abc-worker-0", Phase: "Running", PodIP: "10.244.0.6"},
		{Name: "okdev-s-abc-worker-1", Phase: "Pending"}, // not running: skipped
	}}
	written, err := setupHostAliases(context.Background(), client, "default",
		map[string]string{"okdev.io/managed": "true", "okdev.io/session": "s"}, "dev",
		func(string, ...any) {})
	if err != nil || written != 2 {
		t.Fatalf("written=%d err=%v", written, err)
	}
	script := client.scripts["okdev-s-abc-master-0"]
	for _, want := range []string{"10.244.0.5 master-0", "10.244.0.6 worker-0", "/etc/hosts"} {
		if !strings.Contains(script, want) {
			t.Fatalf("script missing %q:\n%s", want, script)
		}
	}
	if strings.Contains(script, "worker-1") {
		t.Fatalf("pending pod must not be in the alias map:\n%s", script)
	}
}

func TestSetupHostAliasesSkipsSinglePod(t *testing.T) {
	client := &fakeHostsClient{pods: []kube.PodSummary{
		{Name: "okdev-s-abc", Phase: "Running", PodIP: "10.244.0.5"},
	}}
	written, err := setupHostAliases(context.Background(), client, "default", map[string]string{"okdev.io/session": "s"}, "dev", func(string, ...any) {})
	if err != nil || written != 0 {
		t.Fatalf("single-pod session must skip, written=%d err=%v", written, err)
	}
	if len(client.scripts) != 0 {
		t.Fatalf("no writes expected, got %v", client.scripts)
	}
}
