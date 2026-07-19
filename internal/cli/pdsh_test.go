package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	k8sexec "k8s.io/client-go/util/exec"
	utilexec "k8s.io/utils/exec"
)

func TestFilterPodsByRole(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "master-0", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "worker-1", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}
	got := filterPodsByRole(pods, "worker")
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-1" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestFilterPodsByLabels(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Labels: map[string]string{"gpu": "a100", "zone": "us"}},
		{Name: "b", Labels: map[string]string{"gpu": "a100", "zone": "eu"}},
		{Name: "c", Labels: map[string]string{"gpu": "h100", "zone": "us"}},
	}
	got := filterPodsByLabels(pods, []string{"gpu=a100", "zone=us"})
	if len(got) != 1 || got[0].Name != "a" {
		t.Fatalf("expected pod a, got %v", got)
	}
}

func TestResolvePodAliases(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "trainjob-abc-master-0"},
		{Name: "trainjob-abc-worker-0"},
		{Name: "trainjob-abc-worker-1"},
	}

	// Full names, status short names, and unique suffixes all resolve.
	got, err := resolvePodAliases(pods, []string{"trainjob-abc-master-0", "worker-1"})
	if err != nil || len(got) != 2 || got[0].Name != "trainjob-abc-master-0" || got[1].Name != "trainjob-abc-worker-1" {
		t.Fatalf("unexpected resolution: %v err=%v", got, err)
	}
	got, err = resolvePodAliases(pods, []string{"master-0"})
	if err != nil || len(got) != 1 || got[0].Name != "trainjob-abc-master-0" {
		t.Fatalf("short name must resolve, got %v err=%v", got, err)
	}

	// Duplicates collapse.
	got, err = resolvePodAliases(pods, []string{"worker-0", "trainjob-abc-worker-0"})
	if err != nil || len(got) != 1 {
		t.Fatalf("expected dedup, got %v err=%v", got, err)
	}

	// Unknown names error with available aliases instead of silently
	// dropping (the old filter returned a smaller set).
	if _, err := resolvePodAliases(pods, []string{"worker-9"}); err == nil || !strings.Contains(err.Error(), "master-0") {
		t.Fatalf("expected unknown-name error listing aliases, got %v", err)
	}

	// Ambiguous suffixes error and list candidates.
	if _, err := resolvePodAliases(pods, []string{"0"}); err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestExcludePodsAliases(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "trainjob-abc-master-0"},
		{Name: "trainjob-abc-worker-0"},
	}
	got := excludePods(pods, []string{"worker-0"})
	if len(got) != 1 || got[0].Name != "trainjob-abc-master-0" {
		t.Fatalf("short-name exclusion failed: %v", got)
	}
	// Unknown exclusions are tolerated (pod may no longer exist).
	got = excludePods(pods, []string{"gone-pod"})
	if len(got) != 2 {
		t.Fatalf("unknown exclusion must be a no-op, got %v", got)
	}
}

func TestExcludePods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0"},
		{Name: "worker-1"},
		{Name: "worker-2"},
	}
	got := excludePods(pods, []string{"worker-1"})
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
	if got[0].Name != "worker-0" || got[1].Name != "worker-2" {
		t.Fatalf("unexpected pods: %v", got)
	}
}

func TestDetachCommand(t *testing.T) {
	spec := newDetachJobSpec("job-123", "okdev-sess-worker-0", "dev", "python train.py --epochs 100", []string{"python", "train.py", "--epochs", "100"}, nil)
	got := detachCommand(spec)
	if len(got) != 3 {
		t.Fatalf("expected shell command triplet, got %v", got)
	}
	if got[0] != "sh" || got[1] != "-c" {
		t.Fatalf("unexpected launcher prefix: %v", got)
	}
	script := got[2]
	for _, want := range []string{
		// The launcher prefers the shared runtime volume and falls back to
		// the legacy /tmp dir when the volume is absent or not writable
		// (e.g. non-root user on a user-supplied root-owned volume).
		"dir='" + detachMetadataDir + "'",
		"dir='" + legacyDetachMetadataDir + "'",
		"[ ! -w \"$dir\" ]",
		"mkdir -p \"$dir\"",
		"wrapper_path=\"$dir\"/'job-123.sh'",
		// The wrapper is written through sed so the dir placeholder in its
		// baked-in paths resolves to the launcher's chosen directory.
		"sed \"s|" + detachDirPlaceholder + "|$dir|g\" >\"$wrapper_path\" <<'OKDEV_DETACH_WRAPPER'",
		"chmod 700 \"$wrapper_path\"",
		"nohup sh \"$wrapper_path\" >\"$log_path\" 2>&1 &",
		"set -- 'python' 'train.py' '--epochs' '100'",
		"meta_path='" + detachDirPlaceholder + "/job-123.json'",
		"rc=$?",
		"state\":\"running\"",
		"state\":\"exited\"",
		"printf '%s\\n' \"$launch_json\"",
		// The launch record echoed to the caller carries the resolved dir.
		"s|" + detachDirPlaceholder + "|$dir|g; s/__OKDEV_DETACH_PID__/$pid/g",
		// Launcher must block until the wrapper publishes its initial
		// metadata so a caller-followed `okdev exec-jobs` always sees the
		// new job.
		"while [ ! -e \"$meta_path\" ]",
		// Wrapper owns the completion write through an EXIT trap so a
		// fast-exiting user command cannot have its 'exited' record
		// clobbered by a later 'running' write from the launcher.
		"trap 'rc=$?",
		"' EXIT",
		// The user command runs via setsid so it leads its own process
		// group (pgid == pid) and `jobs stop` can signal the whole tree;
		// `(exec CMD) &` remains the fallback for images without setsid,
		// recording pgid=0 so stop degrades to leader-only signaling.
		"if command -v setsid >/dev/null 2>&1; then",
		"setsid \"$@\" &",
		"user_pgid=$user_pid",
		"(exec \"$@\") &",
		"user_pgid=0",
		"user_pid=$!",
		"wait \"$user_pid\"",
		// Both metadata writes resolve the pgid placeholder.
		"s/__OKDEV_DETACH_PGID__/$user_pgid/g",
		"\"pgid\":__OKDEV_DETACH_PGID__",
		// OKDEV_JOB_ID is exported so exec-jobs can reconcile liveness via
		// /proc/<pid>/environ without relying on unique cmdlines.
		"export OKDEV_JOB_ID='job-123'",
		// Launcher reports the user command's pid, which it reads back from
		// the wrapper-written metadata rather than using $! (which is the
		// wrapper's pid).
		"sed -n 's/.*\"pid\":\\([0-9][0-9]*\\).*/\\1/p' \"$meta_path\"",
	} {
		if !strings.Contains(script, want) {
			t.Fatalf("expected detach script to contain %q, got %q", want, script)
		}
	}
	// The launcher must NOT write metadata itself; only the wrapper does.
	// Any "$meta_json" reference would indicate a regression of the race.
	if strings.Contains(script, "$meta_json") {
		t.Fatalf("launcher should not write metadata directly; saw $meta_json reference in %q", script)
	}
	// The wrapper must not `set -e` around the user command, otherwise a
	// non-zero exit would skip the completion write.
	if strings.Contains(script, "set -eu\npython train.py") || strings.Contains(script, "set -e\npython train.py") {
		t.Fatalf("wrapper should not run user command under set -e; got %q", script)
	}
}

func TestDetachCommandWithQuotes(t *testing.T) {
	spec := detachJobSpec{
		JobID:      "job-quoted",
		Pod:        "worker-0",
		Container:  "dev",
		Command:    "echo 'hello world'",
		Argv:       []string{"echo", "hello world"},
		StartedAt:  "2026-04-23T03:00:00Z",
		LogPath:    detachDirPlaceholder + "/job-quoted.log",
		MetaPath:   detachDirPlaceholder + "/job-quoted.json",
		ScriptPath: detachDirPlaceholder + "/job-quoted.sh",
	}
	got := detachCommand(spec)
	if len(got) != 3 {
		t.Fatalf("expected shell command triplet, got %v", got)
	}
	if !strings.Contains(got[2], "nohup sh \"$wrapper_path\"") || !strings.Contains(got[2], "set -- 'echo' 'hello world'") || !strings.Contains(got[2], "(exec \"$@\") &") {
		t.Fatalf("expected quoted user command to survive detach wrapper, got %q", got[2])
	}
}

func TestScriptExecCommandUsesDirectExecForShebangScripts(t *testing.T) {
	got := scriptExecCommand("/tmp/okdev-exec/script.py", true, []string{"--epochs", "3"}, true)
	if len(got) < 5 || got[0] != "sh" || got[1] != "-lc" {
		t.Fatalf("unexpected script exec command shape: %v", got)
	}
	if !strings.Contains(got[2], "\"$script_path\" \"$@\"") {
		t.Fatalf("expected direct exec wrapper, got %q", got[2])
	}
	if !strings.Contains(got[2], "rm -f \"$script_path\"") {
		t.Fatalf("expected cleanup for foreground script exec, got %q", got[2])
	}
}

func TestScriptExecCommandFallsBackToShWithoutShebang(t *testing.T) {
	got := scriptExecCommand("/tmp/okdev-exec/script.sh", false, []string{"--epochs", "3"}, true)
	if len(got) < 5 || got[0] != "sh" || got[1] != "-lc" {
		t.Fatalf("unexpected script exec command shape: %v", got)
	}
	if !strings.Contains(got[2], "sh \"$script_path\" \"$@\"") {
		t.Fatalf("expected sh fallback wrapper, got %q", got[2])
	}
}

func TestBuildExecInvocationWithScriptDetectsShebang(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "remote.py")
	if err := os.WriteFile(scriptPath, []byte("#!/usr/bin/env python3\nprint('ok')\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	got, err := buildExecInvocation([]string{"--epochs", "3"}, scriptPath)
	if err != nil {
		t.Fatalf("buildExecInvocation: %v", err)
	}
	if got.ScriptLocalPath != scriptPath {
		t.Fatalf("script path = %q, want %q", got.ScriptLocalPath, scriptPath)
	}
	if !got.ScriptHasShebang {
		t.Fatal("expected shebang detection")
	}
	if !strings.Contains(got.DisplayCommand, scriptPath) {
		t.Fatalf("display command should include script path, got %q", got.DisplayCommand)
	}
}

func TestValidateExecModeRejectsShellWithScript(t *testing.T) {
	err := validateExecMode("bash", execInvocation{ScriptLocalPath: "/tmp/run.sh"})
	if err == nil || !strings.Contains(err.Error(), "--shell cannot be used with --script") {
		t.Fatalf("expected shell/script conflict, got %v", err)
	}
}

func TestValidateExecModeAllowsWhitespaceShellWithScript(t *testing.T) {
	// A shell value of just whitespace should be treated as unset so it does
	// not collide with --script. Otherwise users who export an empty SHELL
	// would hit a spurious error.
	if err := validateExecMode("   ", execInvocation{ScriptLocalPath: "/tmp/run.sh"}); err != nil {
		t.Fatalf("expected whitespace shell to be ignored, got %v", err)
	}
}

func TestBuildExecInvocationRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	if _, err := buildExecInvocation(nil, dir); err == nil || !strings.Contains(err.Error(), "script path must be a file") {
		t.Fatalf("expected directory rejection, got %v", err)
	}
}

func TestBuildExecInvocationRejectsMissingFile(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "does-not-exist.sh")
	_, err := buildExecInvocation(nil, missing)
	if err == nil || !strings.Contains(err.Error(), "stat script") {
		t.Fatalf("expected stat error for missing script, got %v", err)
	}
}

func TestScriptExecCommandInstallsCleanupTrap(t *testing.T) {
	// The foreground wrapper must remove the uploaded script even if the
	// remote shell is signalled mid-run. Verify the trap is wired up and
	// preserves the user script's exit status.
	got := scriptExecCommand("/tmp/okdev-exec-script-XYZ", true, nil, true)
	if len(got) < 5 || got[0] != "sh" || got[1] != "-lc" {
		t.Fatalf("unexpected wrapper shape: %v", got)
	}
	body := got[2]
	if !strings.Contains(body, "trap 'rc=$?; rm -f \"$script_path\" 2>/dev/null; exit $rc' EXIT INT TERM") {
		t.Fatalf("expected cleanup trap wired to EXIT/INT/TERM, got %q", body)
	}
}

func TestScriptExecCommandSkipsTrapWithoutCleanup(t *testing.T) {
	got := scriptExecCommand("/tmp/okdev-exec-script-XYZ", true, nil, false)
	if strings.Contains(got[2], "trap ") {
		t.Fatalf("expected no trap when cleanup is disabled, got %q", got[2])
	}
}

func TestRemoteExecScriptPathLivesInTmpWithStablePrefix(t *testing.T) {
	// Stable, greppable prefix lets operators find leftover uploads after a
	// SIGKILL bypasses the trap. /tmp avoids depending on a parent dir on
	// the target container.
	p1 := remoteExecScriptPath()
	p2 := remoteExecScriptPath()
	if !strings.HasPrefix(p1, "/tmp/okdev-exec-script-") {
		t.Fatalf("expected /tmp/okdev-exec-script- prefix, got %q", p1)
	}
	if p1 == p2 {
		t.Fatalf("expected unique paths per call, got duplicate %q", p1)
	}
}

func TestDetachWrapperScriptWiresCleanup(t *testing.T) {
	// The detach EXIT trap must invoke cleanup() so per-pod uploaded scripts
	// are removed once the user command exits.
	got := detachWrapperScript("job-x", []string{"echo", "hi"}, []string{"/tmp/okdev-exec-script-A"}, "/tmp/okdev-exec/job-x.json", "RUNNING", "EXITED")
	if !strings.Contains(got, "rm -f '/tmp/okdev-exec-script-A' 2>/dev/null || true") {
		t.Fatalf("expected cleanup line for uploaded script, got %q", got)
	}
	if !strings.Contains(got, "cleanup; exit $rc") {
		t.Fatalf("expected EXIT trap to call cleanup, got %q", got)
	}
}

func TestDetachWrapperScriptIsValidShellWithAndWithoutCleanup(t *testing.T) {
	// Regression: without any cleanup paths (the common non-script
	// `okdev exec --detach` invocation), the wrapper previously emitted
	// `cleanup() {\n}` which is a POSIX sh syntax error. The wrapper
	// aborted before publishing metadata and the launcher surfaced
	// "N of N pods failed to detach" 5 seconds later. Validate both
	// branches with `sh -n` so the regression cannot reappear.
	cases := []struct {
		name    string
		cleanup []string
	}{
		{name: "no cleanup paths", cleanup: nil},
		{name: "one cleanup path", cleanup: []string{"/tmp/okdev-exec-script-A"}},
		{name: "blank cleanup path", cleanup: []string{"   "}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			script := detachWrapperScript("job-x", []string{"echo", "hi"}, tc.cleanup, "/tmp/okdev-exec/job-x.json", "RUNNING", "EXITED")
			cmd := exec.Command("sh", "-n")
			cmd.Stdin = strings.NewReader(script)
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err := cmd.Run(); err != nil {
				t.Fatalf("wrapper script failed sh -n syntax check: %v\nstderr: %s\nscript:\n%s", err, stderr.String(), script)
			}
		})
	}
}

func TestFilterRunningPods(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "a", Phase: "Running"},
		{Name: "b", Phase: "Pending"},
		{Name: "c", Phase: "Running"},
	}
	got := filterRunningPods(pods)
	if len(got) != 2 {
		t.Fatalf("expected 2 pods, got %d", len(got))
	}
}

func TestRunMultiExecFailureSummaryIncludesTerminationHint(t *testing.T) {
	// A dead container's FAILED line should carry the termination cause
	// (OOMKilled + exit + timestamp) instead of a bare kubelet error.
	client := &fakeContainerExecClient{
		errs: map[string]error{
			"worker-0|pytorch": errors.New(`container not found ("pytorch")`),
		},
		restartInfo: map[string]*kube.ContainerRestartInfo{
			"worker-0|pytorch": {
				LastReason:     "OOMKilled",
				LastExitCode:   137,
				LastFinishedAt: time.Date(2026, 7, 5, 6, 32, 11, 0, time.UTC),
			},
		},
	}
	pods := []kube.PodSummary{{Name: "worker-0", Phase: "Running"}}
	var stdout, stderr bytes.Buffer
	sem := make(chan struct{}, 1)
	err := runMultiExec(context.Background(), client, "default", pods, "pytorch", []string{"true"}, "", 0, true, sem, &stdout, &stderr)
	if err == nil {
		t.Fatal("expected failure")
	}
	if !strings.Contains(stderr.String(), `container "pytorch" terminated: OOMKilled (exit 137, finished 2026-07-05T06:32:11Z)`) {
		t.Fatalf("expected termination hint in FAILED summary, got %q", stderr.String())
	}
}

func TestNewDetachJobSpecPlacesFilesOnRuntimeVolume(t *testing.T) {
	// Detached job logs and metadata must prefer the okdev-runtime volume
	// (shared with the sidecar) so they survive a target-container crash;
	// /tmp is container-private overlay and dies with the container. The
	// spec carries a placeholder dir that the launcher resolves on the pod.
	if detachMetadataDir != "/var/okdev/exec" {
		t.Fatalf("detach metadata dir must be on the shared runtime volume, got %q", detachMetadataDir)
	}
	if legacyDetachMetadataDir != "/tmp/okdev-exec" {
		t.Fatalf("legacy detach metadata dir must stay stable for fallback/compat, got %q", legacyDetachMetadataDir)
	}
	spec := newDetachJobSpec("", "pod-1", "dev", "python train.py", []string{"python", "train.py"}, nil)
	for _, p := range []string{spec.LogPath, spec.MetaPath, spec.ScriptPath} {
		if !strings.HasPrefix(p, detachDirPlaceholder+"/") {
			t.Fatalf("expected placeholder-dir path, got %q", p)
		}
	}
}

type fakePdshExecClient struct {
	mu      sync.Mutex
	calls   []string
	cmds    map[string][]string
	uploads map[string]string
	outputs map[string]string
	errs    map[string]error
}

func (f *fakePdshExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	f.mu.Lock()
	f.calls = append(f.calls, pod)
	if f.cmds == nil {
		f.cmds = make(map[string][]string)
	}
	f.cmds[pod] = append([]string(nil), command...)
	output := f.outputs[pod]
	err := f.errs[pod]
	f.mu.Unlock()
	if output != "" {
		fmt.Fprint(stdout, output)
	}
	return err
}

func (f *fakePdshExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return f.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

func (f *fakePdshExecClient) CopyToPodInContainer(ctx context.Context, namespace, localPath, podName, container, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.uploads == nil {
		f.uploads = make(map[string]string)
	}
	f.uploads[podName] = remotePath
	return nil
}

type slowExecClient struct {
	delay time.Duration
}

func (s *slowExecClient) ExecInteractive(ctx context.Context, namespace, pod string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(s.delay):
		return nil
	}
}

func (s *slowExecClient) ExecInteractiveInContainer(ctx context.Context, namespace, pod, container string, tty bool, command []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) error {
	return s.ExecInteractive(ctx, namespace, pod, tty, command, stdin, stdout, stderr)
}

func TestRunDetachExec(t *testing.T) {
	launch, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-shared",
		PID:      48217,
		LogPath:  "/tmp/okdev-exec/job-worker-0.log",
		MetaPath: "/tmp/okdev-exec/job-worker-0.json",
	})
	launch2, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-shared",
		PID:      49102,
		LogPath:  "/tmp/okdev-exec/job-worker-1.log",
		MetaPath: "/tmp/okdev-exec/job-worker-1.json",
	})
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": string(launch) + "\n",
			"okdev-sess-worker-1": string(launch2) + "\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", execInvocation{
		Argv:           []string{"python", "train.py"},
		DisplayCommand: "python train.py",
	}, make(chan struct{}, pdshDefaultFanout), &stdout)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got := stdout.String()
	for _, want := range []string{
		"worker-0] detached job_id=job-shared pid=48217 log=/tmp/okdev-exec/job-worker-0.log meta=/tmp/okdev-exec/job-worker-0.json",
		"worker-1] detached job_id=job-shared pid=49102 log=/tmp/okdev-exec/job-worker-1.log meta=/tmp/okdev-exec/job-worker-1.json",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected detach metadata output %q, got %q", want, got)
		}
	}
}

func TestRunDetachExecWithError(t *testing.T) {
	launch, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-worker-0",
		PID:      48217,
		LogPath:  "/tmp/okdev-exec/job-worker-0.log",
		MetaPath: "/tmp/okdev-exec/job-worker-0.json",
	})
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": string(launch) + "\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": errors.New("pod not ready"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", execInvocation{
		Argv:           []string{"python", "train.py"},
		DisplayCommand: "python train.py",
	}, make(chan struct{}, pdshDefaultFanout), &stdout)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	if !strings.Contains(stdout.String(), "worker-0] detached job_id=job-worker-0 pid=48217") {
		t.Fatalf("expected detach for worker-0, got %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "worker-1] error:") {
		t.Fatalf("expected error for worker-1, got %q", stdout.String())
	}
}

func TestRunDetachExecRequiresLaunchMetadata(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "detached\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
	}
	var stdout bytes.Buffer
	err := runDetachExec(context.Background(), client, "default", pods, "dev", execInvocation{
		Argv:           []string{"python", "train.py"},
		DisplayCommand: "python train.py",
	}, make(chan struct{}, pdshDefaultFanout), &stdout)
	if err == nil {
		t.Fatal("expected metadata parse error")
	}
	if !strings.Contains(stdout.String(), "worker-0] error: missing detach launch metadata") {
		t.Fatalf("expected metadata parse error to be surfaced, got %q", stdout.String())
	}
}

func TestRunMultiExecScriptUploadsPerPod(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("echo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
			"okdev-sess-worker-1": "ok\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	invocation, err := buildExecInvocation([]string{"--epochs", "3"}, scriptPath)
	if err != nil {
		t.Fatalf("buildExecInvocation: %v", err)
	}

	var stdout, stderr bytes.Buffer
	if err := runMultiExecScript(context.Background(), client, "default", pods, "dev", invocation, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr); err != nil {
		t.Fatalf("runMultiExecScript: %v", err)
	}
	for _, pod := range []string{"okdev-sess-worker-0", "okdev-sess-worker-1"} {
		remotePath := client.uploads[pod]
		if remotePath == "" {
			t.Fatalf("expected upload for %s", pod)
		}
		cmd := client.cmds[pod]
		if len(cmd) < 5 || cmd[0] != "sh" || cmd[1] != "-lc" {
			t.Fatalf("unexpected command for %s: %v", pod, cmd)
		}
		if !strings.Contains(cmd[2], "sh \"$script_path\" \"$@\"") {
			t.Fatalf("expected shell-script wrapper for %s, got %q", pod, cmd[2])
		}
		if cmd[4] != remotePath {
			t.Fatalf("expected remote payload path in command args for %s, got %v", pod, cmd)
		}
	}
}

func TestRunDetachExecUploadsScriptPerPod(t *testing.T) {
	dir := t.TempDir()
	scriptPath := filepath.Join(dir, "run.sh")
	if err := os.WriteFile(scriptPath, []byte("echo ok\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	launch, _ := json.Marshal(detachLaunchInfo{
		JobID:    "job-worker-0",
		PID:      48217,
		LogPath:  "/tmp/okdev-exec/job-worker-0.log",
		MetaPath: "/tmp/okdev-exec/job-worker-0.json",
	})
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": string(launch) + "\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
	}
	invocation, err := buildExecInvocation([]string{"--epochs", "3"}, scriptPath)
	if err != nil {
		t.Fatalf("buildExecInvocation: %v", err)
	}

	var stdout bytes.Buffer
	if err := runDetachExec(context.Background(), client, "default", pods, "dev", invocation, make(chan struct{}, pdshDefaultFanout), &stdout); err != nil {
		t.Fatalf("runDetachExec: %v", err)
	}
	remotePath := client.uploads["okdev-sess-worker-0"]
	if remotePath == "" {
		t.Fatal("expected uploaded script path")
	}
	cmd := client.cmds["okdev-sess-worker-0"]
	if len(cmd) != 3 || cmd[0] != "sh" || cmd[1] != "-c" {
		t.Fatalf("unexpected detach launcher command: %v", cmd)
	}
	if !strings.Contains(cmd[2], remotePath) {
		t.Fatalf("expected detach launcher to reference uploaded path %q, got %q", remotePath, cmd[2])
	}
	if !strings.Contains(cmd[2], "set -- 'sh' '-lc'") {
		t.Fatalf("expected detach wrapper to preserve script argv, got %q", cmd[2])
	}
}

func TestRunMultiExecSuccess(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
			"okdev-sess-worker-1": "ok\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !strings.Contains(stdout.String(), "worker-0] ok") || !strings.Contains(stdout.String(), "worker-1] ok") {
		t.Fatalf("expected prefixed output, got %q", stdout.String())
	}
}

func TestRunMultiExecPartialFailure(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "ok\n",
		},
		errs: map[string]error{
			"okdev-sess-worker-1": k8sexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1},
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "echo ok"}, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for partial failure")
	}
	// A pure remote non-zero exit is a command result, not an okdev
	// failure: softer framing, plain error (exit 1), no infra sentinel.
	if !strings.Contains(err.Error(), "command exited non-zero on 1 of 2 pod(s)") {
		t.Fatalf("unexpected error message: %v", err)
	}
	if errors.Is(err, ErrExecInfraFailure) {
		t.Fatalf("remote-exit-only run must not carry the infra sentinel: %v", err)
	}
	got := stderr.String()
	if !strings.Contains(got, "\nCOMMAND EXITED NON-ZERO:\n") || strings.Contains(got, "FAILED:") {
		t.Fatalf("expected command-result framing without FAILED header, got %q", got)
	}
	if !strings.Contains(got, "pod=okdev-sess-worker-1 container=dev kind=remote-exit exit=1") {
		t.Fatalf("expected classified failure summary for worker-1, got %q", got)
	}
}

func TestRunMultiExecSinglePodPreservesRemoteExitStatus(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{},
		errs: map[string]error{
			"okdev-sess-master-0": k8sexec.CodeExitError{Err: errors.New("command terminated with exit code 7"), Code: 7},
		},
	}
	pods := []kube.PodSummary{{Name: "okdev-sess-master-0", Phase: "Running"}}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-lc", "exit 7"}, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for non-zero remote exit")
	}
	// The documented contract: a single-pod remote exit status is preserved
	// verbatim, so the wrapped ExitError must survive for main's exit mapper.
	var exitErr utilexec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitStatus() != 7 {
		t.Fatalf("expected wrapped ExitError with status 7, got %v", err)
	}
	if errors.Is(err, ErrExecInfraFailure) {
		t.Fatalf("remote-exit-only run must not carry the infra sentinel: %v", err)
	}
	if got := stderr.String(); !strings.Contains(got, "\nCOMMAND EXITED NON-ZERO:\n") || strings.Contains(got, "FAILED:") {
		t.Fatalf("expected command-result framing without FAILED header, got %q", got)
	}
}

func TestRunMultiExecClassifiesTimeoutAndTransportFailures(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{},
		errs: map[string]error{
			"okdev-sess-worker-0": context.DeadlineExceeded,
			"okdev-sess-worker-1": errors.New("stream error: INTERNAL_ERROR"),
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "serving", []string{"sh", "-lc", "echo ok"}, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected error for classified failures")
	}
	got := stderr.String()
	if !strings.Contains(got, "pod=okdev-sess-worker-0 container=serving kind=timeout") {
		t.Fatalf("expected timeout classification, got %q", got)
	}
	if !strings.Contains(got, "pod=okdev-sess-worker-1 container=serving kind=transport") {
		t.Fatalf("expected transport classification, got %q", got)
	}
}

func TestRunMultiExecTimeout(t *testing.T) {
	slowClient := &slowExecClient{delay: 5 * time.Second}
	pods := []kube.PodSummary{
		{Name: "pod-0", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), slowClient, "default", pods, "dev", []string{"sh", "-c", "sleep 100"}, "", 50*time.Millisecond, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err == nil {
		t.Fatal("expected timeout error")
	}
}

func TestRunMultiExecNoPrefix(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"pod-0": "hello\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "pod-0", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-c", "echo hello"}, "", 0, true, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := stdout.String(); got != "hello\n" {
		t.Fatalf("expected raw output, got %q", got)
	}
}

func TestRunMultiExecWithLogDir(t *testing.T) {
	client := &fakePdshExecClient{
		outputs: map[string]string{
			"okdev-sess-worker-0": "gpu0\n",
			"okdev-sess-worker-1": "gpu1\n",
		},
		errs: map[string]error{},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	logDir := t.TempDir()
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"sh", "-c", "echo gpu"}, logDir, 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, name := range []string{"worker-0.log", "worker-1.log"} {
		data, err := os.ReadFile(filepath.Join(logDir, name))
		if err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		if len(data) == 0 {
			t.Fatalf("%s is empty", name)
		}
	}
}

func TestValidateMultiPodFlags(t *testing.T) {
	tests := []struct {
		name       string
		allPods    bool
		workers    bool
		podNames   []string
		role       string
		labels     []string
		exclude    []string
		groups     []string
		hasCmd     bool
		detach     bool
		sequential bool
		parallel   bool
		wantErr    string
	}{
		{
			name:    "role requires command",
			role:    "worker",
			wantErr: "command after -- or --script is required",
		},
		{
			name:    "detach requires cmd",
			detach:  true,
			wantErr: "command after -- or --script is required",
		},
		{
			name:    "exclude without selector",
			exclude: []string{"p1"},
			wantErr: "command after -- or --script is required",
		},
		{
			name:     "exclude with pod and command",
			podNames: []string{"p1"}, exclude: []string{"p1"}, hasCmd: true,
			wantErr: "--exclude cannot be used with --pod",
		},
		{
			name:    "workers conflicts with role",
			workers: true,
			role:    "worker",
			hasCmd:  true,
			wantErr: "--workers cannot be used with --role",
		},
		{
			name:     "group conflicts with pod",
			groups:   []string{"worker-0,worker-1"},
			podNames: []string{"p1"},
			hasCmd:   true,
			wantErr:  "--group cannot be used with --pod, --role, --label, --exclude, --all, or --workers",
		},
		{
			name:       "sequential conflicts with parallel",
			hasCmd:     true,
			sequential: true,
			parallel:   true,
			wantErr:    "--parallel and --sequential are mutually exclusive",
		},
		{
			name:     "parallel requires groups",
			hasCmd:   true,
			parallel: true,
			wantErr:  "--parallel requires one or more --group flags",
		},
		{
			name:       "sequential without command",
			sequential: true,
			wantErr:    "command after -- or --script is required",
		},
		{
			name:    "all without command",
			allPods: true,
			wantErr: "command after -- or --script is required",
		},
		{
			name:    "group without command",
			groups:  []string{"worker-0"},
			wantErr: "command after -- or --script is required",
		},
		{
			name:   "valid default all with command",
			hasCmd: true,
		},
		{
			name: "valid role with cmd",
			role: "worker", hasCmd: true,
		},
		{
			name:    "valid exclude with default all",
			exclude: []string{"p1"}, hasCmd: true,
		},
		{
			name:   "valid detach with cmd",
			detach: true, hasCmd: true,
		},
		{
			name:    "valid workers with command",
			workers: true, hasCmd: true,
		},
		{
			name:   "valid groups with sequential command",
			groups: []string{"master-0,worker-0", "master-1,worker-1"},
			hasCmd: true, sequential: true,
		},
		{
			name:   "valid groups with parallel command",
			groups: []string{"master-0,worker-0", "master-1,worker-1"},
			hasCmd: true, parallel: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateMultiPodFlags(tt.allPods, tt.workers, tt.podNames, tt.role, tt.labels, tt.exclude, tt.groups, tt.hasCmd, tt.detach, tt.sequential, tt.parallel)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestValidateNoFlagLikeValues(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "group swallows following flag",
			args:    []string{"--group", "--detach"},
			wantErr: `--group value "--detach" begins with "-"`,
		},
		{
			name:    "pod swallows following flag",
			args:    []string{"--pod", "--all"},
			wantErr: `--pod value "--all" begins with "-"`,
		},
		{
			name:    "string flag swallows following flag",
			args:    []string{"--role", "--all"},
			wantErr: `--role value "--all" begins with "-"`,
		},
		{
			name:    "swallowed separator gets missing-value message",
			args:    []string{"--group", "--"},
			wantErr: `missing value for --group`,
		},
		{
			name:    "script path hint keeps full value",
			args:    []string{"--script", "--detach"},
			wantErr: `"./--detach"`,
		},
		{
			name:    "log-dir path hint keeps full value",
			args:    []string{"--log-dir", "-x"},
			wantErr: `"./-x"`,
		},
		{
			name:    "equals form also rejected",
			args:    []string{"--group=--detach"},
			wantErr: `--group value "--detach" begins with "-"`,
		},
		{
			name:    "dash item inside comma list rejected",
			args:    []string{"--pod", "worker-0,-1"},
			wantErr: `--pod value "-1" begins with "-"`,
		},
		{
			name: "defaults untouched",
			args: []string{},
		},
		{
			name: "valid values pass",
			args: []string{"--group", "master-0,worker-0", "--script", "./collect.sh", "--label", "tier=gpu", "--pod", "worker-0"},
		},
		{
			name: "dash-leading script escaped with dot-slash passes",
			args: []string{"--script", "./-weird.sh"},
		},
		{
			name: "non-string flags are ignored",
			args: []string{"--detach", "--fanout", "5"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExecCmd(&Options{})
			if err := cmd.ParseFlags(tt.args); err != nil {
				t.Fatalf("ParseFlags: %v", err)
			}
			err := validateNoFlagLikeValues(cmd.Flags())
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
		})
	}
}

func TestPkillExecCommandShape(t *testing.T) {
	argv := pkillExecCommand("train.py", "TERM")
	if len(argv) != 6 || argv[0] != "sh" || argv[1] != "-c" || argv[3] != "okdev-pkill" || argv[4] != "train.py" || argv[5] != "TERM" {
		t.Fatalf("unexpected argv: %#v", argv)
	}
	// The script must exclude self+ancestors and follow pkill exit semantics.
	for _, want := range []string{"excl=\" $$ \"", "^PPid:", `[ "$matched" -eq 1 ]`, "kill -s"} {
		if !strings.Contains(argv[2], want) {
			t.Fatalf("helper script missing %q:\n%s", want, argv[2])
		}
	}
	// The pattern must never be embedded in the script text itself.
	if strings.Contains(argv[2], "train.py") {
		t.Fatalf("pattern leaked into script text:\n%s", argv[2])
	}
}

func TestExecPkillFlagValidation(t *testing.T) {
	tests := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{
			name:    "pkill with command conflicts",
			args:    []string{"--pkill", "train.py", "--", "echo", "hi"},
			wantErr: "--pkill cannot be combined with a command",
		},
		{
			name:    "pkill with script conflicts",
			args:    []string{"--pkill", "train.py", "--script", "./x.sh"},
			wantErr: "--pkill cannot be combined with a command after -- or --script",
		},
		{
			name:    "pkill with shell conflicts",
			args:    []string{"--pkill", "train.py", "--shell", "bash"},
			wantErr: "--pkill cannot be used with --shell",
		},
		{
			name:    "pkill with detach conflicts",
			args:    []string{"--pkill", "train.py", "--detach"},
			wantErr: "--pkill cannot be used with --detach",
		},
		{
			name:    "signal without pkill",
			args:    []string{"--signal", "KILL", "--", "echo", "hi"},
			wantErr: "--pkill requires a non-empty pattern",
		},
		{
			name:    "invalid signal",
			args:    []string{"--pkill", "train.py", "--signal", "TERM;rm"},
			wantErr: "must be a signal name or number",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExecCmd(&Options{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
			}
		})
	}
}

// Runtime behavior of the --pkill helper (issue #170 follow-up): it must kill
// a process whose cmdline matches the pattern, spare unrelated processes, and
// spare itself even though its own argv carries the pattern. Linux-only: the
// helper walks /proc.
func TestPkillHelperKillsMatchAndSparesSelf(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("pkill helper walks /proc; linux only")
	}
	marker := fmt.Sprintf("okdev-pkill-decoy-%d-%d", os.Getpid(), time.Now().UnixNano())

	// The trailing `:` keeps sleep off the script's tail position — busybox
	// ash tail-call execs the last command, which would replace the shell
	// and drop the marker from the decoy's cmdline.
	decoy := exec.Command("sh", "-c", ": "+marker+"; sleep 30; :")
	if err := decoy.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = decoy.Process.Kill() }()

	bystander := exec.Command("sh", "-c", ": okdev-pkill-bystander; sleep 30; :")
	if err := bystander.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = bystander.Process.Kill() }()

	argv := pkillExecCommand(marker, "TERM")
	out, err := exec.Command(argv[0], argv[1:]...).Output()
	if err != nil {
		t.Fatalf("helper failed (self-kill or no match): %v, output %q", err, out)
	}
	if !strings.Contains(string(out), fmt.Sprintf("killed %d", decoy.Process.Pid)) {
		t.Fatalf("expected decoy pid %d in kill report, got %q", decoy.Process.Pid, out)
	}

	werr := decoy.Wait()
	if werr == nil {
		t.Fatal("decoy exited cleanly; expected SIGTERM")
	}
	if bystander.Process.Signal(syscall.Signal(0)) != nil {
		t.Fatal("bystander was killed; helper matched too broadly")
	}

	// No match: pkill convention exit 1.
	argv = pkillExecCommand(marker+"-no-such-process", "TERM")
	if err := exec.Command(argv[0], argv[1:]...).Run(); err == nil {
		t.Fatal("expected non-zero exit when nothing matches")
	}
}

// Regression for issue #177: the validation must win the race against
// validateExecArgs. A swallowed "--" turns command words into session args, so
// without Args-stage ordering the user saw "accepts at most 1 session
// argument before --" instead of the real cause.
func TestExecRejectsFlagLikeValuesBeforeArgsValidation(t *testing.T) {
	tests := []struct {
		name       string
		args       []string
		wantErr    string
		notWantErr string
	}{
		{
			name:       "forgotten group value swallows separator",
			args:       []string{"--detach", "--group", "--", "python", "train.py"},
			wantErr:    "missing value for --group",
			notWantErr: "accepts at most 1 session argument",
		},
		{
			name:       "group swallows following flag",
			args:       []string{"--group", "--detach", "--", "true"},
			wantErr:    `--group value "--detach" begins with "-"`,
			notWantErr: "unknown pod",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cmd := newExecCmd(&Options{})
			cmd.SetOut(&bytes.Buffer{})
			cmd.SetErr(&bytes.Buffer{})
			cmd.SetArgs(tt.args)
			err := cmd.Execute()
			if err == nil {
				t.Fatal("expected error")
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("expected error containing %q, got %q", tt.wantErr, err.Error())
			}
			if strings.Contains(err.Error(), tt.notWantErr) {
				t.Fatalf("error %q must not contain %q", err.Error(), tt.notWantErr)
			}
		})
	}
}

func TestBuildExecPodGroupsSupportsShortNamesAndWorkers(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "okdev-sess-master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Master"}},
		{Name: "okdev-sess-worker-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
		{Name: "okdev-sess-worker-1", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "Worker"}},
	}

	groups, err := buildExecPodGroups("sess", pods, execTargetPlan{
		workers:    true,
		sequential: true,
	})
	if err != nil {
		t.Fatalf("buildExecPodGroups workers: %v", err)
	}
	if len(groups) != 1 || len(groups[0].Pods) != 2 {
		t.Fatalf("expected a single group with 2 worker pods, got %+v", groups)
	}
	if groups[0].Pods[0].Name != "okdev-sess-worker-0" || groups[0].Pods[1].Name != "okdev-sess-worker-1" {
		t.Fatalf("unexpected worker pods: %+v", groups[0].Pods)
	}

	grouped, err := buildExecPodGroups("sess", pods, execTargetPlan{
		groups: []string{"master-0,worker-1"},
	})
	if err != nil {
		t.Fatalf("buildExecPodGroups groups: %v", err)
	}
	if len(grouped) != 1 || len(grouped[0].Pods) != 2 {
		t.Fatalf("unexpected grouped selection: %+v", grouped)
	}
	if grouped[0].Pods[0].Name != "okdev-sess-master-0" || grouped[0].Pods[1].Name != "okdev-sess-worker-1" {
		t.Fatalf("unexpected grouped pods: %+v", grouped[0].Pods)
	}
}

func TestBuildExecPodGroupsRejectsPodInMultipleGroups(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "okdev-sess-master-0", Phase: "Running"},
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	_, err := buildExecPodGroups("sess", pods, execTargetPlan{
		groups: []string{"master-0,worker-0", "master-0,worker-1"},
	})
	if err == nil || !strings.Contains(err.Error(), `appears in both group 1 and group 2`) {
		t.Fatalf("expected duplicate pod error, got %v", err)
	}
}

func TestFilterRunningPodsReadinessCheck(t *testing.T) {
	allPods := []kube.PodSummary{
		{Name: "leader-0", Phase: "Running"},
		{Name: "worker-0", Phase: "Pending"},
		{Name: "worker-1", Phase: "ContainerCreating"},
	}
	running := filterRunningPods(allPods)

	// Without --ready-only, should detect the gap.
	if len(running) == len(allPods) {
		t.Fatal("expected fewer running pods than total")
	}
	if len(running) != 1 {
		t.Fatalf("expected 1 running pod, got %d", len(running))
	}
	if running[0].Name != "leader-0" {
		t.Fatalf("expected leader-0, got %s", running[0].Name)
	}
}

func TestResolveExecNoPrefix(t *testing.T) {
	// A non-terminal writer (bytes.Buffer has no Fd) simulates a pipe/redirect.
	var pipe bytes.Buffer

	tests := []struct {
		name          string
		explicitlySet bool
		noPrefix      bool
		out           io.Writer
		want          bool
	}{
		{name: "piped, flag untouched -> auto-suppress", explicitlySet: false, noPrefix: false, out: &pipe, want: true},
		{name: "piped, explicit --no-prefix=false honored", explicitlySet: true, noPrefix: false, out: &pipe, want: false},
		{name: "piped, explicit --no-prefix=true", explicitlySet: true, noPrefix: true, out: &pipe, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveExecNoPrefix(tt.explicitlySet, tt.noPrefix, tt.out); got != tt.want {
				t.Fatalf("resolveExecNoPrefix(%v, %v, pipe) = %v, want %v", tt.explicitlySet, tt.noPrefix, got, tt.want)
			}
		})
	}
}

func TestRunMultiExecInfraFailureCarriesSentinel(t *testing.T) {
	// Mixed remote-exit + timeout: any delivery failure keeps the FAILED
	// framing and maps to the dedicated infra exit code so monitors can
	// tell it apart from a command that merely exited non-zero.
	client := &fakePdshExecClient{
		errs: map[string]error{
			"okdev-sess-worker-0": k8sexec.CodeExitError{Err: errors.New("command terminated with exit code 1"), Code: 1},
			"okdev-sess-worker-1": context.DeadlineExceeded,
		},
	}
	pods := []kube.PodSummary{
		{Name: "okdev-sess-worker-0", Phase: "Running"},
		{Name: "okdev-sess-worker-1", Phase: "Running"},
	}
	var stdout, stderr bytes.Buffer
	err := runMultiExec(context.Background(), client, "default", pods, "dev", []string{"true"}, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
	if err == nil || !errors.Is(err, ErrExecInfraFailure) {
		t.Fatalf("expected infra sentinel, got %v", err)
	}
	if code, ok := ClassifiedExitCode(err); !ok || code != ExitExecInfraFailure {
		t.Fatalf("expected exit code %d, got %d ok=%v", ExitExecInfraFailure, code, ok)
	}
	if !strings.Contains(stderr.String(), "FAILED:") {
		t.Fatalf("expected FAILED framing for infra failures, got %q", stderr.String())
	}
}

// Regression for issue #170: an exit 143/137 from a command containing
// pkill/kill gets a self-kill hint on stderr (the pattern likely matched a
// shell running the command itself), while unrelated commands and exit codes
// stay hint-free.
func TestRunMultiExecSelfKillHint(t *testing.T) {
	tests := []struct {
		name     string
		command  []string
		exitCode int
		wantHint bool
	}{
		{
			name:     "pkill with SIGTERM exit gets hint",
			command:  []string{"bash", "-lc", `pkill -f "train.py"; rm -f /tmp/lock`},
			exitCode: 143,
			wantHint: true,
		},
		{
			name:     "kill with SIGKILL exit gets hint",
			command:  []string{"bash", "-lc", "kill -9 $(cat pid)"},
			exitCode: 137,
			wantHint: true,
		},
		{
			name:     "pkill with ordinary exit gets no hint",
			command:  []string{"bash", "-lc", `pkill -f "train.py"`},
			exitCode: 1,
			wantHint: false,
		},
		{
			name:     "signal exit without kill in command gets no hint",
			command:  []string{"python", "train.py"},
			exitCode: 143,
			wantHint: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client := &fakePdshExecClient{
				errs: map[string]error{
					"okdev-sess-worker-0": k8sexec.CodeExitError{Err: fmt.Errorf("command terminated with exit code %d", tt.exitCode), Code: tt.exitCode},
				},
			}
			pods := []kube.PodSummary{{Name: "okdev-sess-worker-0", Phase: "Running"}}
			var stdout, stderr bytes.Buffer
			err := runMultiExec(context.Background(), client, "default", pods, "dev", tt.command, "", 0, false, make(chan struct{}, pdshDefaultFanout), &stdout, &stderr)
			if err == nil {
				t.Fatal("expected error")
			}
			gotHint := strings.Contains(stderr.String(), "hint: exit")
			if gotHint != tt.wantHint {
				t.Fatalf("hint presence = %v, want %v; stderr %q", gotHint, tt.wantHint, stderr.String())
			}
			if tt.wantHint && !strings.Contains(stderr.String(), "patter[n]") {
				t.Fatalf("expected bracket-trick suggestion, got %q", stderr.String())
			}
		})
	}
}
