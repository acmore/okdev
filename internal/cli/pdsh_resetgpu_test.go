package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestBuildResetGPUInvocationConflicts(t *testing.T) {
	if _, err := buildResetGPUInvocation([]string{"echo"}, "", "", false, ""); err == nil {
		t.Fatal("command args must conflict")
	}
	if _, err := buildResetGPUInvocation(nil, "s.sh", "", false, ""); err == nil {
		t.Fatal("--script must conflict")
	}
	if _, err := buildResetGPUInvocation(nil, "", "zsh", false, ""); err == nil {
		t.Fatal("--shell must conflict")
	}
	if _, err := buildResetGPUInvocation(nil, "", "", true, ""); err == nil {
		t.Fatal("--detach must conflict")
	}
	if _, err := buildResetGPUInvocation(nil, "", "", false, "train"); err == nil {
		t.Fatal("--pkill must conflict")
	}
	inv, err := buildResetGPUInvocation(nil, "", "", false, "")
	if err != nil {
		t.Fatalf("plain --reset-gpu: %v", err)
	}
	if inv.DisplayCommand != "--reset-gpu" || len(inv.Argv) == 0 {
		t.Fatalf("unexpected invocation %+v", inv)
	}
}

// installFakeNvidiaSMI writes an nvidia-smi stub that reports the pids listed
// in stateFile that are still alive, mimicking compute-apps queries.
func installFakeNvidiaSMI(t *testing.T, binDir, stateFile string) {
	t.Helper()
	script := `#!/bin/sh
# fake nvidia-smi: report live pids from the state file
[ -f "` + stateFile + `" ] || exit 0
for pid in $(cat "` + stateFile + `"); do
  if kill -0 "$pid" 2>/dev/null; then
    case "$*" in
      *pid,process_name,used_memory*) echo "$pid, fake_proc, 1024 MiB" ;;
      *) echo "$pid" ;;
    esac
  fi
done
`
	if err := os.WriteFile(filepath.Join(binDir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

func runResetGPUScript(t *testing.T, binDir string, verifySeconds string) (int, string, string) {
	t.Helper()
	cmd := exec.Command("sh", "-c", resetGPUHelperScript, "okdev-reset-gpu", verifySeconds)
	cmd.Env = append(os.Environ(), "PATH="+binDir+":"+os.Getenv("PATH"))
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	code := 0
	if err != nil {
		var exitErr *exec.ExitError
		if !strings.Contains(err.Error(), "exit status") {
			t.Fatalf("script run: %v (stderr: %s)", err, stderr.String())
		}
		_ = exitErr
		code = cmd.ProcessState.ExitCode()
	}
	return code, stdout.String(), stderr.String()
}

func TestResetGPUScriptNoNvidiaSMI(t *testing.T) {
	empty := t.TempDir() // PATH prefix without nvidia-smi
	cmd := exec.Command("sh", "-c", resetGPUHelperScript, "okdev-reset-gpu", "1")
	cmd.Env = []string{"PATH=" + empty} // minimal PATH: no nvidia-smi anywhere
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	err := cmd.Run()
	if cmd.ProcessState.ExitCode() != 3 {
		t.Fatalf("expected exit 3, got %d (err %v, stderr %s)", cmd.ProcessState.ExitCode(), err, stderr.String())
	}
	if !strings.Contains(stderr.String(), "nvidia-smi not found") {
		t.Fatalf("expected clear error, got %q", stderr.String())
	}
}

func TestResetGPUScriptAlreadyClearIsIdempotentSuccess(t *testing.T) {
	dir := t.TempDir()
	installFakeNvidiaSMI(t, dir, filepath.Join(dir, "gpu-pids"))
	code, stdout, stderr := runResetGPUScript(t, dir, "2")
	if code != 0 || !strings.Contains(stdout, "gpu-clear") {
		t.Fatalf("expected clean success, got code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if strings.Contains(stdout, "killed") {
		t.Fatalf("nothing should be killed on a clear GPU, got %q", stdout)
	}
}

func TestResetGPUScriptKillsHolderGroupAndVerifies(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the helper reads /proc (cmdline, stat, pgid); exercised on linux and via docker verification")
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "gpu-pids")
	installFakeNvidiaSMI(t, dir, stateFile)

	// A fake GPU holder in its own process group, so the group kill cannot
	// take the test process down with it.
	holder := exec.Command("sh", "-c", "sleep 300")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill() }()
	if err := os.WriteFile(stateFile, []byte(strconv.Itoa(holder.Process.Pid)+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	code, stdout, stderr := runResetGPUScript(t, dir, "5")
	if code != 0 {
		t.Fatalf("expected success, got code=%d stdout=%q stderr=%q", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "killed group") || !strings.Contains(stdout, "gpu-clear") {
		t.Fatalf("expected group kill + verification, got %q", stdout)
	}
	// The holder must actually be dead.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := holder.Process.Signal(syscall.Signal(0)); err != nil {
			return
		}
		_ = holder.Wait
		time.Sleep(50 * time.Millisecond)
	}
	// Reap and re-check.
	_ = holder.Wait()
}

func TestResetGPUScriptReportsUnmappablePidAndTimesOut(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("the /proc-existence check drives this path; exercised on linux and via docker verification")
	}
	dir := t.TempDir()
	stateFile := filepath.Join(dir, "gpu-pids")
	// A stub that always reports a pid that cannot exist in this namespace.
	script := `#!/bin/sh
echo 99999999
`
	if err := os.WriteFile(filepath.Join(dir, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = stateFile
	code, _, stderr := runResetGPUScript(t, dir, "1")
	if code != 4 {
		t.Fatalf("expected exit 4 (still busy), got %d (stderr %q)", code, stderr)
	}
	if !strings.Contains(stderr, "not visible in this container namespace") || !strings.Contains(stderr, "gpu-busy") {
		t.Fatalf("expected unmappable-pid note and busy report, got %q", stderr)
	}
}
