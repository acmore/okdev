package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

// startMarkedProcess starts a long-running process that carries marker in its
// cmdline and belongs to the given worktree ("" for none), in its own process
// group. The returned channel closes when it has exited and been reaped.
func startMarkedProcess(t *testing.T, marker, worktree string) (*exec.Cmd, <-chan struct{}) {
	return startMarkedProcessInGroup(t, marker, worktree, 0)
}

func startMarkedProcessInGroup(t *testing.T, marker, worktree string, group int) (*exec.Cmd, <-chan struct{}) {
	t.Helper()
	// The trailing `:` keeps sleep off the tail position so the shell — and
	// with it the marker — stays in the process's cmdline.
	cmd := exec.Command("sh", "-c", ": "+marker+"; sleep 300; :")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true, Pgid: group}
	cmd.Env = os.Environ()
	if worktree != "" {
		cmd.Env = append(cmd.Env, "OKDEV_WORKTREE_ID="+worktree)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		_ = cmd.Wait()
		close(done)
	}()
	t.Cleanup(func() {
		if group == 0 {
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		} else {
			_ = cmd.Process.Kill()
		}
	})
	return cmd, done
}

func exited(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	case <-time.After(3 * time.Second):
		return false
	}
}

func alive(done <-chan struct{}) bool {
	select {
	case <-done:
		return false
	case <-time.After(300 * time.Millisecond):
		return true
	}
}

// Parallel agents share one pod. A pattern kill issued from one worktree must
// not take down another worktree's process that happens to match.
func TestPkillFromAWorktreeSparesOtherWorktrees(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the helper reads /proc/<pid>/environ; linux only")
	}
	marker := fmt.Sprintf("okdev-scope-decoy-%d-%d", os.Getpid(), time.Now().UnixNano())
	_, mine := startMarkedProcess(t, marker, "wt-a")
	_, theirs := startMarkedProcess(t, marker, "wt-b")
	_, primary := startMarkedProcess(t, marker, "")

	run := func(allWorktrees bool, worktree string) {
		t.Helper()
		argv := pkillExecCommand(marker, "KILL", allWorktrees)
		cmd := exec.Command(argv[0], argv[1:]...)
		cmd.Env = os.Environ()
		if worktree != "" {
			cmd.Env = append(cmd.Env, "OKDEV_WORKTREE_ID="+worktree)
		}
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("pkill helper: %v: %s", err, out)
		}
	}
	run(false, "wt-a")
	if !exited(mine) {
		t.Fatal("the worktree's own process survived")
	}
	if !alive(theirs) || !alive(primary) {
		t.Fatal("a process of another worktree or the primary checkout was killed")
	}
	run(true, "wt-a")
	if !exited(theirs) || !exited(primary) {
		t.Fatal("--all-worktrees left a matching process alive")
	}
}

func TestResetGPUFromAWorktreeLeavesOtherHoldersAlone(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the helper reads /proc; linux only")
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "gpu-pids")
	installFakeNvidiaSMI(t, dir, stateFile)
	mineCmd, mine := startMarkedProcess(t, "gpu-holder", "wt-a")
	theirsCmd, theirs := startMarkedProcess(t, "gpu-holder", "wt-b")
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf("%d\n%d\n", mineCmd.Process.Pid, theirsCmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}

	run := func(scope string) (int, string, string) {
		t.Helper()
		cmd := exec.Command("sh", "-c", resetGPUHelperScript, "okdev-reset-gpu", "5", scope)
		cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "OKDEV_WORKTREE_ID=wt-a")
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		_ = cmd.Run()
		return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
	}
	code, stdout, stderr := run("")
	if code != 0 || !strings.Contains(stdout, "gpu-clear: worktree wt-a") || !strings.Contains(stderr, "1 process(es) of other worktrees") {
		t.Fatalf("scoped reset: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !exited(mine) || !alive(theirs) {
		t.Fatal("scoped reset did not kill exactly the worktree's own holder")
	}
	code, stdout, stderr = run("all")
	if code != 0 || strings.TrimSpace(stdout[strings.LastIndex(strings.TrimSpace(stdout), "\n")+1:]) != "gpu-clear" || !exited(theirs) {
		t.Fatalf("session-wide reset: code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
}

func TestResetGPUFromAWorktreeDoesNotKillOtherGroupMembers(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the helper reads /proc; linux only")
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "gpu-pids")
	installFakeNvidiaSMI(t, dir, stateFile)
	mineCmd, mine := startMarkedProcess(t, "gpu-holder", "wt-a")
	_, theirs := startMarkedProcessInGroup(t, "other-worktree", "wt-b", mineCmd.Process.Pid)
	if err := os.WriteFile(stateFile, []byte(fmt.Sprintf("%d\n", mineCmd.Process.Pid)), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("sh", "-c", resetGPUHelperScript, "okdev-reset-gpu", "5", "")
	cmd.Env = append(os.Environ(), "PATH="+dir+":"+os.Getenv("PATH"), "OKDEV_WORKTREE_ID=wt-a")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("scoped reset: %v: %s", err, out)
	}
	if !exited(mine) || !alive(theirs) {
		t.Fatal("scoped reset killed a process from another worktree in the same process group")
	}
}

func TestGPURequestValidation(t *testing.T) {
	command := execInvocation{Argv: []string{"python", "train.py"}}
	for name, tc := range map[string]struct {
		invocation execInvocation
		gpus       string
		wait       time.Duration
		waitSet    bool
		killMode   bool
		transport  string
		wantErr    string
	}{
		"count":             {invocation: command, gpus: "2"},
		"all":               {invocation: command, gpus: "all", wait: time.Minute, waitSet: true},
		"none":              {invocation: command},
		"zero":              {invocation: command, gpus: "0", wantErr: "positive number"},
		"word":              {invocation: command, gpus: "both", wantErr: "positive number"},
		"wait without":      {invocation: command, waitSet: true, wantErr: "--gpu-wait requires --gpus"},
		"negative wait":     {invocation: command, gpus: "1", wait: -time.Second, wantErr: "negative"},
		"with kill":         {invocation: command, gpus: "1", killMode: true, wantErr: "--pkill or --reset-gpu"},
		"interactive":       {gpus: "1", wantErr: "needs a command"},
		"ssh transport":     {invocation: command, gpus: "1", transport: "ssh", wantErr: "--transport=ssh"},
		"script as command": {invocation: execInvocation{ScriptLocalPath: "run.sh"}, gpus: "1"},
	} {
		invocation := tc.invocation
		err := applyGPURequest(&invocation, tc.gpus, tc.wait, tc.waitSet, tc.killMode, tc.transport)
		switch {
		case tc.wantErr == "" && (err != nil || invocation.GPUs != tc.gpus || invocation.GPUWait != tc.wait):
			t.Errorf("%s: invocation = %+v, err = %v", name, invocation, err)
		case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
			t.Errorf("%s: err = %v, want %q", name, err, tc.wantErr)
		}
	}
}

// The lease is taken inside the worktree wrapper, so the helper sees which
// worktree is asking; and a pod whose sidecar predates the helper says so.
func TestGPUWrapperOrderAndOldSidecar(t *testing.T) {
	inv := execInvocation{WorkDir: "/var/okdev/worktrees/0123456789abcdef", WorktreeID: "0123456789abcdef", GPUs: "2", GPUWait: 90 * time.Second}
	argv := inv.wrap([]string{"python", "train.py"})
	joined := strings.Join(argv, "\x00")
	worktreeAt, gpusAt := strings.Index(joined, "okdev-worktree"), strings.Index(joined, "okdev-gpus")
	if worktreeAt < 0 || gpusAt < worktreeAt {
		t.Fatalf("wrappers out of order: %q", argv)
	}
	if tail := argv[len(argv)-5:]; tail[0] != gpuLeaseHelper || tail[1] != "2" || tail[2] != "1m30s" || tail[3] != "python" || tail[4] != "train.py" {
		t.Fatalf("lease arguments = %q", tail)
	}
	if got := (execInvocation{}).wrap([]string{"true"}); len(got) != 1 {
		t.Fatalf("plain command was wrapped: %q", got)
	}

	if _, err := os.Stat(gpuLeaseHelper); err == nil {
		t.Skip("the lease helper exists on this machine")
	}
	wrapped := commandWithGPUs([]string{"true"}, "1", 0)
	cmd := exec.Command(wrapped[0], wrapped[1:]...)
	out, _ := cmd.CombinedOutput()
	if cmd.ProcessState.ExitCode() != 69 || !strings.Contains(string(out), "newer okdev sidecar") {
		t.Fatalf("old sidecar: exit = %d, output = %q", cmd.ProcessState.ExitCode(), out)
	}
}

func TestJobsListingIsScopedToTheWorktree(t *testing.T) {
	job := func(id, worktree string) string {
		tag := ""
		if worktree != "" {
			tag = `"worktree":"` + worktree + `",`
		}
		return `{"jobId":"` + id + `","pod":"pod-0","container":"dev",` + tag + `"pid":100,"command":"python ` + id + `.py","startedAt":"2026-04-23T03:00:00Z","stdoutPath":"/x.log","stderrPath":"/x.log","metaPath":"/x.json","state":"running"}` + "\n"
	}
	client := &fakePdshExecClient{
		outputs: map[string]string{"pod-0": job("job-a", "wt-a") + job("job-b", "wt-b") + job("job-main", "")},
		errs:    map[string]error{},
	}
	pods := []kube.PodSummary{{Name: "pod-0", Phase: "Running"}}
	list := func(scope execJobsScope) string {
		t.Helper()
		var out bytes.Buffer
		if err := runExecJobsScoped(context.Background(), client, "default", pods, "dev", "", pdshDefaultFanout, &out, false, fanoutRoute{}, scope); err != nil {
			t.Fatal(err)
		}
		return out.String()
	}

	scoped := list(execJobsScope{worktree: "wt-a"})
	if !strings.Contains(scoped, "job-a") || strings.Contains(scoped, "job-b") || strings.Contains(scoped, "job-main") || !strings.Contains(scoped, "2 job(s) from other worktrees") {
		t.Fatalf("scoped listing = %q", scoped)
	}
	all := list(execJobsScope{showWorktree: true})
	for _, want := range []string{"WORKTREE", "job-a", "wt-a", "job-b", "wt-b", "job-main"} {
		if !strings.Contains(all, want) {
			t.Fatalf("session listing misses %q: %q", want, all)
		}
	}
	if plain := list(execJobsScope{}); strings.Contains(plain, "WORKTREE") || !strings.Contains(plain, "job-b") {
		t.Fatalf("primary listing = %q", plain)
	}
}
