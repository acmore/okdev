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
	corev1 "k8s.io/api/core/v1"
)

type fakeStatusDetailsClient struct {
	describe string
	results  map[string]error
	pods     map[string]*corev1.Pod
}

func (f fakeStatusDetailsClient) DescribePod(_ context.Context, namespace, pod string) (string, error) {
	return f.describe + " " + namespace + "/" + pod, nil
}

func (f fakeStatusDetailsClient) GetPod(_ context.Context, namespace, pod string) (*corev1.Pod, error) {
	if p, ok := f.pods[namespace+"/"+pod]; ok {
		return p.DeepCopy(), nil
	}
	return nil, os.ErrNotExist
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
				PodIP:     "10.0.0.10",
				NodeName:  "gpu-node-a",
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
	detail := gatherDetailedStatus(context.Background(), nil, cfg, "/tmp/.okdev.yaml", "default", view, fakeStatusDetailsClient{
		describe: "details",
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": nil,
			`test -f "$HOME"/.codex/auth.json`: nil,
		},
		pods: map[string]*corev1.Pod{
			"default/worker-0": {
				Spec: corev1.PodSpec{
					NodeName: "gpu-node-a",
					Volumes: []corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
						{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}},
					},
					Containers: []corev1.Container{
						{
							Name: "trainer",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
								{Name: "data", MountPath: "/data"},
							},
						},
					},
				},
				Status: corev1.PodStatus{PodIP: "10.0.0.10"},
			},
			"default/helper-0": {
				Spec: corev1.PodSpec{
					NodeName: "gpu-node-b",
					Volumes: []corev1.Volume{
						{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
					},
					Containers: []corev1.Container{
						{
							Name: "trainer",
							VolumeMounts: []corev1.VolumeMount{
								{Name: "workspace", MountPath: "/workspace"},
							},
						},
					},
				},
				Status: corev1.PodStatus{PodIP: "10.0.0.11"},
			},
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
	if len(detail.PathSemantics) != 1 {
		t.Fatalf("unexpected path semantics: %+v", detail.PathSemantics)
	}
	if detail.PathSemantics[0].SyncScope != "all-pods-via-syncthing" {
		t.Fatalf("unexpected sync scope: %+v", detail.PathSemantics[0])
	}
	if detail.PathSemantics[0].Survival != "ephemeral in pod" {
		t.Fatalf("unexpected survival: %+v", detail.PathSemantics[0])
	}
	if detail.Sync.ConflictCount != 1 || len(detail.Sync.ConflictPaths) != 1 || !strings.Contains(detail.Sync.ConflictPaths[0], "sync-conflict") {
		t.Fatalf("unexpected sync conflicts: %+v", detail.Sync)
	}
	var selectedPod *detailedStatusPod
	for i := range detail.Pods {
		if detail.Pods[i].Name == "worker-0" {
			selectedPod = &detail.Pods[i]
			break
		}
	}
	if selectedPod == nil || selectedPod.PodIP != "10.0.0.10" || selectedPod.NodeName != "gpu-node-a" {
		t.Fatalf("unexpected pod location detail: %+v", detail.Pods)
	}
	if len(selectedPod.Mounts) == 0 {
		t.Fatalf("expected mount details, got %+v", selectedPod)
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

	_ = gatherDetailedStatus(context.Background(), nil, cfg, "/tmp/.okdev.yaml", "default", view, nil)

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
		Pods: []detailedStatusPod{{
			Name:     "worker-0",
			Phase:    "Running",
			Ready:    "1/1",
			Age:      "1h",
			Selected: true,
			PodIP:    "10.0.0.10",
			NodeName: "gpu-node-a",
			Mounts: []detailedStatusMount{
				{Name: "workspace", MountPath: "/workspace", SourceType: "emptyDir", Persistence: "ephemeral"},
				{Name: "data", MountPath: "/data", SourceType: "hostPath", Persistence: "persistent"},
			},
		}},
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
		PathSemantics: []detailedStatusPathSemantics{
			{
				LocalPath:     "./",
				RemotePath:    "/workspace",
				WorkspacePath: "/workspace",
				SyncScope:     "all-pods-via-syncthing",
				Survival:      "ephemeral in pod",
			},
			{
				LocalPath:     "./logs",
				RemotePath:    "/data/logs",
				WorkspacePath: "/workspace",
				SyncScope:     "not-shared-by-sync",
				Survival:      "survives session restart",
			},
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
		"ip=10.0.0.10",
		"node=gpu-node-a",
		"mounts: /workspace [emptyDir, ephemeral], /data [hostPath, persistent]",
		"SSH:",
		"managed forward: running",
		"Sync:",
		"background: running (pid 123)",
		"conflicts: 2",
		"./a.sync-conflict-1",
		"Path Semantics:",
		"./ -> /workspace sync=all-pods-via-syncthing survival=ephemeral in pod",
		"./logs -> /data/logs sync=not-shared-by-sync survival=survives session restart",
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

func TestClassifyMountSourceAndPersistence(t *testing.T) {
	tests := []struct {
		name        string
		volume      corev1.Volume
		wantSource  string
		wantPersist string
	}{
		{
			name:        "emptyDir",
			volume:      corev1.Volume{Name: "workspace", VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
			wantSource:  "emptyDir",
			wantPersist: "ephemeral",
		},
		{
			name:        "hostPath",
			volume:      corev1.Volume{Name: "data", VolumeSource: corev1.VolumeSource{HostPath: &corev1.HostPathVolumeSource{Path: "/data"}}},
			wantSource:  "hostPath",
			wantPersist: "persistent",
		},
		{
			name:        "persistent volume claim",
			volume:      corev1.Volume{Name: "pvc", VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "workspace"}}},
			wantSource:  "persistentVolumeClaim",
			wantPersist: "persistent",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotSource, gotPersist := classifyMountSource(tt.volume)
			if gotSource != tt.wantSource || gotPersist != tt.wantPersist {
				t.Fatalf("classifyMountSource() = (%q, %q), want (%q, %q)", gotSource, gotPersist, tt.wantSource, tt.wantPersist)
			}
		})
	}
}

func TestBuildPathSemanticsUsesLongestMatchingMount(t *testing.T) {
	pairs := []detailedStatusPathPair{
		{LocalPath: "./", RemotePath: "/workspace"},
		{LocalPath: "./logs", RemotePath: "/data/logs"},
	}
	mounts := []detailedStatusMount{
		{Name: "root", MountPath: "/", SourceType: "other", Persistence: "unknown"},
		{Name: "workspace", MountPath: "/workspace", SourceType: "emptyDir", Persistence: "ephemeral"},
		{Name: "data", MountPath: "/data", SourceType: "hostPath", Persistence: "persistent"},
	}

	got := buildPathSemantics(pairs, mounts, "/workspace")
	if len(got) != 2 {
		t.Fatalf("expected 2 path semantics rows, got %+v", got)
	}
	if got[0].SyncScope != "all-pods-via-syncthing" || got[0].Survival != "ephemeral in pod" {
		t.Fatalf("unexpected workspace path semantics: %+v", got[0])
	}
	if got[1].SyncScope != "not-shared-by-sync" || got[1].Survival != "survives session restart" {
		t.Fatalf("unexpected data path semantics: %+v", got[1])
	}
}

func TestPrintDetailedStatusIncludesMeshHealth(t *testing.T) {
	var out bytes.Buffer
	printDetailedStatus(&out, detailedStatus{
		Session:   "sess1",
		Namespace: "default",
		Owner:     "me",
		Workload:  "pytorchjob",
		Phase:     "Running",
		Ready:     "1/1",
		Age:       "2h",
		Target: detailedStatusTarget{
			SelectedPod:       "trainer-0",
			SelectedContainer: "dev",
		},
		SSH: detailedStatusSSH{HostAlias: "okdev-sess1", ManagedForwardStatus: "running"},
		Sync: detailedStatusSync{
			Engine:           "syncthing",
			BackgroundStatus: "running",
			MeshLines:        []string{"topology: hub-and-spoke", "hub: trainer-0", "receivers: 2"},
			MeshHealth: &meshHealthSummary{
				HubPod:   "trainer-0",
				FolderID: "okdev-sess1",
				Receivers: []meshReceiverHealth{
					{Pod: "worker-0", Connected: true, InSync: true},
					{Pod: "worker-1", Connected: false},
				},
			},
		},
		Logs: detailedStatusLogs{OKDevLog: "/tmp/okdev.log"},
	})

	got := out.String()
	for _, want := range []string{
		"Mesh:",
		"topology: hub-and-spoke",
		"hub: trainer-0",
		"1/2 receiver(s) healthy",
		"worker-0: synced",
		"worker-1: disconnected",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q:\n%s", want, got)
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
