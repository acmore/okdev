package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
	"github.com/acmore/okdev/internal/workload"
)

type fakeStatusDetailsClient struct {
	describe string
	results  map[string]error
}

func (f fakeStatusDetailsClient) DescribePod(_ context.Context, namespace, pod string) (string, error) {
	return f.describe + " " + namespace + "/" + pod, nil
}

func (f fakeStatusDetailsClient) ExecShInContainer(_ context.Context, _, _, _, script string) ([]byte, error) {
	if err, ok := f.results[script]; ok {
		return nil, err
	}
	return nil, nil
}

func (f fakeStatusDetailsClient) CopyToPodInContainer(context.Context, string, string, string, string, string) error {
	return nil
}

func TestGatherDetailedStatusIncludesDiagnostics(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	origForward := sshForwardStatusFunc
	origConfig := sshConfigPresentFunc
	sshForwardStatusFunc = func(string) (bool, error) { return true, nil }
	sshConfigPresentFunc = func(string) (bool, error) { return true, nil }
	t.Cleanup(func() {
		sshForwardStatusFunc = origForward
		sshConfigPresentFunc = origConfig
	})

	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Attach.Container = "dev"
	syncRoot := filepath.Join(t.TempDir(), "sync")
	if err := os.MkdirAll(syncRoot, 0o755); err != nil {
		t.Fatalf("mkdir sync root: %v", err)
	}
	conflictPath := filepath.Join(syncRoot, "file.sync-conflict-20260329-123456-ABC.txt")
	if err := os.WriteFile(conflictPath, []byte("conflict"), 0o644); err != nil {
		t.Fatalf("write conflict file: %v", err)
	}
	cfg.Spec.Sync.Paths = []string{syncRoot + ":/workspace"}
	cfg.Spec.Ports = []config.PortMapping{{Name: "http", Local: 8080, Remote: 80}}
	cfg.Spec.Agents = []config.AgentSpec{{Name: "codex", Auth: &config.AgentAuth{LocalPath: "~/.codex/auth.json"}}}

	if err := session.SaveTarget("sess1", workload.TargetRef{PodName: "worker-0", Container: "trainer"}); err != nil {
		t.Fatalf("save target: %v", err)
	}

	view := sessionView{
		Namespace:    "default",
		Session:      "sess1",
		Owner:        "me",
		WorkloadType: "pod",
		TargetPod:    "worker-0",
		Phase:        "Running",
		Ready:        "1/1",
		CreatedAt:    time.Now().Add(-time.Hour),
		PodCount:     2,
		Pods: []kube.PodSummary{
			{
				Name:      "worker-0",
				Namespace: "default",
				Phase:     "Running",
				Ready:     "1/1",
				CreatedAt: time.Now().Add(-time.Hour),
				Labels: map[string]string{
					"okdev.io/workload-role": "Trainer",
				},
			},
			{
				Name:      "helper-0",
				Namespace: "default",
				Phase:     "Running",
				Ready:     "1/1",
				CreatedAt: time.Now().Add(-30 * time.Minute),
				Labels: map[string]string{
					"okdev.io/attachable": "false",
				},
			},
		},
	}

	pidPath := filepath.Join(t.TempDir(), "sync.pid")
	if err := os.WriteFile(pidPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("write pid: %v", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatalf("user home dir: %v", err)
	}
	statusPIDPath := filepath.Join(home, ".okdev", "syncthing", "sess1", "sync.pid")
	if err := os.MkdirAll(filepath.Dir(statusPIDPath), 0o755); err != nil {
		t.Fatalf("mkdir status pid dir: %v", err)
	}
	if err := os.WriteFile(statusPIDPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("write status pid: %v", err)
	}
	detail := gatherDetailedStatus(context.Background(), cfg, "/tmp/.okdev.yaml", "default", view, fakeStatusDetailsClient{
		describe: "details",
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": nil,
			`test -f "$HOME"/.codex/auth.json`: nil,
		},
	})

	if detail.Target.SelectedPod != "worker-0" || detail.Target.SelectedContainer != "trainer" {
		t.Fatalf("unexpected selected target: %+v", detail.Target)
	}
	if detail.Target.PinnedPod != "worker-0" || detail.Target.PinnedContainer != "trainer" {
		t.Fatalf("unexpected pinned target: %+v", detail.Target)
	}
	if detail.SSH.ManagedForwardStatus != "running" || !detail.SSH.ConfigPresent {
		t.Fatalf("unexpected ssh detail: %+v", detail.SSH)
	}
	if detail.Sync.BackgroundStatus != "not running" {
		t.Fatalf("unexpected sync background status: %+v", detail.Sync)
	}
	if len(detail.Sync.ConfiguredPaths) != 1 || !strings.Contains(detail.Sync.ConfiguredPaths[0], "/workspace") {
		t.Fatalf("unexpected sync paths: %#v", detail.Sync.ConfiguredPaths)
	}
	if detail.Sync.ConflictCount != 1 || len(detail.Sync.ConflictPaths) != 1 || !strings.Contains(detail.Sync.ConflictPaths[0], "sync-conflict") {
		t.Fatalf("unexpected sync conflicts: %+v", detail.Sync)
	}
	if len(detail.Agents) != 1 || detail.Agents[0].Name != "codex" || !detail.Agents[0].Installed || !detail.Agents[0].AuthStaged {
		t.Fatalf("unexpected agents: %#v", detail.Agents)
	}
	if !strings.Contains(detail.TargetPodDetails, "details default/worker-0") {
		t.Fatalf("unexpected target pod details %q", detail.TargetPodDetails)
	}
}

func TestGatherDetailedStatusDoesNotCreateRuntimeDirectories(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	origForward := sshForwardStatusFunc
	origConfig := sshConfigPresentFunc
	sshForwardStatusFunc = func(string) (bool, error) { return false, nil }
	sshConfigPresentFunc = func(string) (bool, error) { return false, nil }
	t.Cleanup(func() {
		sshForwardStatusFunc = origForward
		sshConfigPresentFunc = origConfig
	})

	cfg := &config.DevEnvironment{}
	view := sessionView{
		Namespace: "default",
		Session:   "sess-no-side-effects",
		TargetPod: "pod-1",
		Pods:      []kube.PodSummary{{Name: "pod-1", Namespace: "default"}},
	}

	_ = gatherDetailedStatus(context.Background(), cfg, "/tmp/.okdev.yaml", "default", view, nil)

	for _, path := range []string{
		filepath.Join(home, ".okdev", "ssh"),
		filepath.Join(home, ".okdev", "syncthing", "sess-no-side-effects"),
	} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("expected %s to remain absent, got err=%v", path, err)
		}
	}
}

func TestBuildDetailedTargetDoesNotMixSelectedPodWithStalePinnedContainer(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveTarget("sess1", workload.TargetRef{PodName: "old-pod", Container: "old-container"}); err != nil {
		t.Fatalf("save target: %v", err)
	}

	cfg := &config.DevEnvironment{}
	cfg.Spec.Workload.Attach.Container = "dev"
	view := sessionView{
		Session:   "sess1",
		TargetPod: "new-pod",
		Pods:      []kube.PodSummary{{Name: "new-pod"}},
	}

	target, _ := buildDetailedTarget(view, cfg)
	if target.SelectedPod != "new-pod" {
		t.Fatalf("unexpected selected pod: %+v", target)
	}
	if target.SelectedContainer != "dev" {
		t.Fatalf("expected selected container to fall back to config target, got %+v", target)
	}
	if target.PinnedContainer != "old-container" {
		t.Fatalf("expected pinned target to remain visible, got %+v", target)
	}
}

func TestPrintDetailedStatusIncludesSections(t *testing.T) {
	var out bytes.Buffer
	printDetailedStatus(&out, detailedStatus{
		Session:   "sess1",
		Namespace: "default",
		Owner:     "me",
		Workload:  "pod",
		Phase:     "Running",
		Ready:     "1/1",
		Age:       "1h",
		Target: detailedStatusTarget{
			SelectedPod:       "worker-0",
			SelectedContainer: "dev",
			PinnedPod:         "worker-0",
			PinnedContainer:   "dev",
		},
		Pods: []detailedStatusPod{{Name: "worker-0", Phase: "Running", Ready: "1/1", Age: "1h", Selected: true}},
		SSH: detailedStatusSSH{
			HostAlias:            "okdev-sess1",
			ConfigPresent:        true,
			ManagedForwardStatus: "running",
			ConfiguredForwards:   []string{"http: localhost:8080 -> remote:80"},
		},
		Sync: detailedStatusSync{
			Engine:             "syncthing",
			BackgroundStatus:   "running",
			BackgroundPID:      123,
			ConfiguredPaths:    []string{"/tmp/repo -> /workspace"},
			PIDPath:            "/tmp/sync.pid",
			LogPath:            "/tmp/syncthing.log",
			LocalHome:          "/tmp/syncthing-home",
			LocalDaemonLogPath: "/tmp/local.log",
			ConflictCount:      2,
			ConflictPaths:      []string{"./a.sync-conflict-1", "./b.sync-conflict-2"},
		},
		Agents:           []agentListRow{{Name: "codex", Installed: true, AuthStaged: true}},
		Logs:             detailedStatusLogs{OKDevLog: "/tmp/okdev.log"},
		TargetPodDetails: "Pod: default/worker-0\nPhase: Running\n",
	})

	got := out.String()
	for _, want := range []string{
		"Session: sess1",
		"Target:",
		"Pods:",
		"SSH:",
		"managed forward: running",
		"Sync:",
		"background: running (pid 123)",
		"conflicts: 2",
		"./a.sync-conflict-1",
		"Agents:",
		"codex: installed=yes authStaged=yes",
		"Logs:",
		"Target Pod Details:",
		"  Pod: default/worker-0",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q: %q", want, got)
		}
	}
}

func TestManagedSSHConfigPresent(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	configPath := filepath.Join(home, ".ssh", "config")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte("# BEGIN OKDEV okdev-sess1\nHost okdev-sess1\n# END OKDEV okdev-sess1\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	ok, err := managedSSHConfigPresent("okdev-sess1")
	if err != nil {
		t.Fatalf("managedSSHConfigPresent: %v", err)
	}
	if !ok {
		t.Fatal("expected managed ssh config block to be found")
	}
}
