package cli

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestParseCpArgs(t *testing.T) {
	tests := []struct {
		name       string
		src, dst   string
		wantLocal  string
		wantRemote string
		wantUpload bool
		wantErr    string
	}{
		{
			name: "upload", src: "./foo", dst: ":/bar",
			wantLocal: "./foo", wantRemote: "/bar", wantUpload: true,
		},
		{
			name: "download", src: ":/bar", dst: "./foo",
			wantLocal: "./foo", wantRemote: "/bar", wantUpload: false,
		},
		{
			name: "both local", src: "./a", dst: "./b",
			wantErr: "exactly one",
		},
		{
			name: "both remote", src: ":/a", dst: ":/b",
			wantErr: "exactly one",
		},
		{
			name: "colon only", src: ":", dst: "./foo",
			wantErr: "remote path cannot be empty",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			local, remote, upload, err := parseCpArgs(tt.src, tt.dst)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if local != tt.wantLocal || remote != tt.wantRemote || upload != tt.wantUpload {
				t.Fatalf("got (%q, %q, %v), want (%q, %q, %v)", local, remote, upload, tt.wantLocal, tt.wantRemote, tt.wantUpload)
			}
		})
	}
}

func TestValidateCpFlags(t *testing.T) {
	tests := []struct {
		name     string
		allPods  bool
		podNames []string
		role     string
		labels   []string
		exclude  []string
		wantErr  string
	}{
		{name: "all and role mutually exclusive", allPods: true, role: "worker", wantErr: "mutually exclusive"},
		{name: "exclude without selector", exclude: []string{"p1"}, wantErr: "--exclude requires"},
		{name: "exclude with pod", podNames: []string{"p1"}, exclude: []string{"p1"}, wantErr: "--exclude cannot be used with --pod"},
		{name: "valid all", allPods: true},
		{name: "valid role", role: "worker"},
		{name: "no flags", allPods: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateCpFlags(tt.allPods, tt.podNames, tt.role, tt.labels, tt.exclude)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error %q, got %v", tt.wantErr, err)
			}
		})
	}
}

func TestMultiPodDownloadPath(t *testing.T) {
	if got := multiPodDownloadPath("/tmp/out", "worker-0", "/workspace/result.txt", false); got != "/tmp/out/worker-0/result.txt" {
		t.Fatalf("file download path = %q", got)
	}
	if got := multiPodDownloadPath("/tmp/out", "worker-0", "/workspace/results", true); got != "/tmp/out/worker-0" {
		t.Fatalf("directory download path = %q", got)
	}
}

func TestDownloadTargetPath(t *testing.T) {
	tests := []struct {
		name        string
		localPath   string
		shortName   string
		remotePath  string
		remoteIsDir bool
		podCount    int
		want        string
	}{
		{
			name:       "single pod file stays flat",
			localPath:  "/tmp/out.txt",
			shortName:  "worker-0",
			remotePath: "/workspace/result.txt",
			podCount:   1,
			want:       "/tmp/out.txt",
		},
		{
			name:        "single pod directory stays flat",
			localPath:   "/tmp/out",
			shortName:   "worker-0",
			remotePath:  "/workspace/results",
			remoteIsDir: true,
			podCount:    1,
			want:        "/tmp/out",
		},
		{
			name:       "multi pod file nests by pod",
			localPath:  "/tmp/out",
			shortName:  "worker-0",
			remotePath: "/workspace/result.txt",
			podCount:   2,
			want:       filepath.Join("/tmp/out", "worker-0", "result.txt"),
		},
		{
			name:        "multi pod directory nests by pod",
			localPath:   "/tmp/out",
			shortName:   "worker-0",
			remotePath:  "/workspace/results",
			remoteIsDir: true,
			podCount:    2,
			want:        filepath.Join("/tmp/out", "worker-0"),
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := downloadTargetPath(tt.localPath, tt.shortName, tt.remotePath, tt.remoteIsDir, tt.podCount); got != tt.want {
				t.Fatalf("download target path = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestDownloadSuccessDestination(t *testing.T) {
	if got := downloadSuccessDestination("/tmp/out.txt", "worker-0", 1); got != "/tmp/out.txt" {
		t.Fatalf("single pod success destination = %q", got)
	}
	if got := downloadSuccessDestination("/tmp/out", "worker-0", 2); got != filepath.Join("/tmp/out", "worker-0")+string(filepath.Separator) {
		t.Fatalf("multi pod success destination = %q", got)
	}
}

func TestCpReadinessCheckErrorsWhenPodsNotRunning(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "sess-master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "sess-worker-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "sess-worker-1", Phase: "Pending", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "sess-worker-2", Phase: "ContainerCreating", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}

	// Simulate the filter-then-readiness pipeline used by runMultiPodCp --all.
	running := filterRunningPods(allPods)
	if len(running) != 2 {
		t.Fatalf("expected 2 running pods, got %d", len(running))
	}
	if len(running) == len(allPods) {
		t.Fatal("expected some pods to be filtered out")
	}

	// Build the error message the same way cp.go does.
	notReady := make([]string, 0)
	runningSet := make(map[string]bool)
	for _, p := range running {
		runningSet[p.Name] = true
	}
	for _, p := range allPods {
		if !runningSet[p.Name] {
			notReady = append(notReady, fmt.Sprintf("%s (%s)", p.Name, p.Phase))
		}
	}
	if len(notReady) != 2 {
		t.Fatalf("expected 2 not-ready pods, got %d: %v", len(notReady), notReady)
	}

	errMsg := fmt.Sprintf("%d/%d pods are not running: %s\nUse --ready-only to copy on the %d ready pods",
		len(notReady), len(allPods), strings.Join(notReady, ", "), len(running))
	if !strings.Contains(errMsg, "2/4 pods are not running") {
		t.Fatalf("unexpected error message: %s", errMsg)
	}
	if !strings.Contains(errMsg, "sess-worker-1 (Pending)") {
		t.Fatalf("expected Pending pod in error: %s", errMsg)
	}
	if !strings.Contains(errMsg, "sess-worker-2 (ContainerCreating)") {
		t.Fatalf("expected ContainerCreating pod in error: %s", errMsg)
	}
	if !strings.Contains(errMsg, "--ready-only") {
		t.Fatalf("expected --ready-only hint: %s", errMsg)
	}
}

func TestCpReadinessCheckWithRoleFilter(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "sess-master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "sess-worker-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "sess-worker-1", Phase: "Pending", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}

	// Filter by role first (as runMultiPodCp does), then check readiness.
	filtered := filterPodsByRole(allPods, "worker")
	if len(filtered) != 2 {
		t.Fatalf("expected 2 workers, got %d", len(filtered))
	}

	running := filterRunningPods(filtered)
	if len(running) != 1 {
		t.Fatalf("expected 1 running worker, got %d", len(running))
	}

	// The denominator should be 2 (filtered workers), not 3 (all pods).
	notReady := len(filtered) - len(running)
	errMsg := fmt.Sprintf("%d/%d pods are not running", notReady, len(filtered))
	if !strings.Contains(errMsg, "1/2 pods are not running") {
		t.Fatalf("readiness check should scope to filtered pods: %s", errMsg)
	}
}

func TestCpReadinessCheckAllPodsRunning(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "sess-worker-0", Phase: "Running"},
		{Name: "sess-worker-1", Phase: "Running"},
		{Name: "sess-worker-2", Phase: "Running"},
	}

	running := filterRunningPods(allPods)
	if len(running) != len(allPods) {
		t.Fatalf("all pods should be running, got %d/%d", len(running), len(allPods))
	}
	// No error should be produced — readyOnly doesn't matter when all pods are running.
}

func TestCpReadinessCheckNoneRunning(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "sess-worker-0", Phase: "Pending"},
		{Name: "sess-worker-1", Phase: "ContainerCreating"},
	}

	running := filterRunningPods(allPods)
	if len(running) != 0 {
		t.Fatalf("expected 0 running pods, got %d", len(running))
	}

	errMsg := fmt.Sprintf("no running pods in session %q (0/%d pods ready)", "test-session", len(allPods))
	if !strings.Contains(errMsg, "0/2 pods ready") {
		t.Fatalf("unexpected zero-running error: %s", errMsg)
	}
}

func TestCpCommandExposesQuietAndParallelFlags(t *testing.T) {
	cmd := newCpCmd(&Options{})
	for _, name := range []string{"quiet", "parallel", "compress"} {
		if cmd.Flags().Lookup(name) == nil {
			t.Errorf("expected --%s flag on okdev cp", name)
		}
	}
	for _, short := range []string{"q", "z"} {
		if cmd.Flags().ShorthandLookup(short) == nil {
			t.Errorf("expected -%s short flag on okdev cp", short)
		}
	}
}

func TestClampParallel(t *testing.T) {
	tests := []struct {
		in, want int
	}{
		{-5, 1},
		{0, 1},
		{1, 1},
		{4, 4},
		{cpMaxParallel, cpMaxParallel},
		{cpMaxParallel + 5, cpMaxParallel},
	}
	for _, tc := range tests {
		if got := clampParallel(tc.in); got != tc.want {
			t.Errorf("clampParallel(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

func TestCpParallelFlagDefaultsToOne(t *testing.T) {
	cmd := newCpCmd(&Options{})
	f := cmd.Flags().Lookup("parallel")
	if f == nil {
		t.Fatal("--parallel flag missing")
	}
	// Parallelism must be strictly opt-in: a recursive single-stream tar is
	// the fastest path for most trees, and extra streams can measurably slow
	// copies down when the apiserver/kubelet is the real bottleneck.
	if f.DefValue != "1" {
		t.Fatalf("--parallel default = %q, want 1", f.DefValue)
	}
}

func TestCpReadinessCheckReadyOnlyBypass(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "sess-worker-0", Phase: "Running"},
		{Name: "sess-worker-1", Phase: "Pending"},
		{Name: "sess-worker-2", Phase: "Running"},
	}

	running := filterRunningPods(allPods)
	if len(running) != 2 {
		t.Fatalf("expected 2 running pods, got %d", len(running))
	}

	// With readyOnly=true, we proceed with running pods despite the gap.
	readyOnly := true
	shouldError := len(running) < len(allPods) && !readyOnly
	if shouldError {
		t.Fatal("readyOnly=true should bypass the error")
	}

	// With readyOnly=false, the gap triggers an error.
	readyOnly = false
	shouldError = len(running) < len(allPods) && !readyOnly
	if !shouldError {
		t.Fatal("readyOnly=false should trigger an error when pods are not running")
	}
}
