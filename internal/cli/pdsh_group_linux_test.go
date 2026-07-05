package cli

import (
	"encoding/json"
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

// Verifies the whole group-kill chain against real processes: the detach
// launcher puts the user command in its own process group via setsid, the
// metadata records the pgid, and the signal script kills the entire tree —
// including children the leader forked (the torchrun/pretrain.py scenario).
// Linux-only: the scripts inspect /proc.
func TestDetachGroupSignalKillsProcessTreeLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("requires /proc and process groups")
	}
	jobID := "job-group-e2e"
	spec := newDetachJobSpec(jobID, "pod-x", "dev", "tree", []string{"sh", "-c", "sleep 300 & sleep 300 & wait"}, nil)
	cmd := detachCommand(spec)
	launcher := exec.Command(cmd[0], cmd[1], cmd[2])
	stdout, err := launcher.Output()
	if err != nil {
		t.Fatalf("launcher failed: %v", err)
	}
	var info detachLaunchInfo
	lines := strings.Split(strings.TrimSpace(string(stdout)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &info); err != nil {
		t.Fatalf("parse launch record %q: %v", stdout, err)
	}
	metaRaw, err := os.ReadFile(info.MetaPath)
	if err != nil {
		t.Fatalf("read metadata: %v", err)
	}
	var meta detachMetadata
	if err := json.Unmarshal(metaRaw, &meta); err != nil {
		t.Fatalf("parse metadata %q: %v", metaRaw, err)
	}
	if meta.PGID <= 0 || meta.PGID != meta.PID {
		t.Fatalf("expected setsid leader with pgid==pid, got %+v", meta)
	}
	defer func() {
		_ = syscall.Kill(-meta.PGID, syscall.SIGKILL)
		dir := filepath.Dir(info.MetaPath)
		for _, f := range []string{info.MetaPath, info.LogPath, filepath.Join(dir, jobID+".sh")} {
			_ = os.Remove(f)
		}
	}()

	// Leader sh + two sleeps: wait for the full tree before killing so the
	// assertion below proves the *children* died, not just the leader.
	waitForGroupSize(t, meta.PGID, 3, 5*time.Second)

	script := signalDetachJobScript(jobID, meta.PID, meta.PGID, "KILL")
	out, err := exec.Command("sh", "-c", script).CombinedOutput()
	if err != nil {
		t.Fatalf("signal script failed: %v\noutput: %s", err, out)
	}
	if !strings.Contains(string(out), "signaled group (3 members)") {
		t.Fatalf("expected group signal confirmation, got %q", out)
	}

	waitForGroupSize(t, meta.PGID, 0, 5*time.Second)
}

func waitForGroupSize(t *testing.T, pgid, want int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		got := countGroupMembers(pgid)
		if got == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d has %d members, want %d", pgid, got, want)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// countGroupMembers counts non-zombie processes in the group by parsing
// /proc/<pid>/stat, mirroring the shell scripts' technique (strip through
// the last ')' to survive spaces/parens in comm).
func countGroupMembers(pgid int) int {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	count := 0
	for _, e := range entries {
		if _, err := strconv.Atoi(e.Name()); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join("/proc", e.Name(), "stat"))
		if err != nil {
			continue
		}
		s := string(data)
		idx := strings.LastIndexByte(s, ')')
		if idx < 0 {
			continue
		}
		fields := strings.Fields(s[idx+1:])
		if len(fields) < 3 || fields[0] == "Z" {
			continue
		}
		if g, err := strconv.Atoi(fields[2]); err == nil && g == pgid {
			count++
		}
	}
	return count
}
