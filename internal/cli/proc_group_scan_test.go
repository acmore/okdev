package cli

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// writeFakeProcStat writes a /proc/<pid>/stat-shaped line: pid, comm in
// parens, then state, ppid, pgrp.
func writeFakeProcStat(t *testing.T, root, pid, comm, state, ppid, pgrp string) string {
	t.Helper()
	dir := filepath.Join(root, pid)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "stat")
	line := strings.Join([]string{pid, "(" + comm + ")", state, ppid, pgrp, "0", "-1", "4194304"}, " ") + "\n"
	if err := os.WriteFile(path, []byte(line), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func runScanScript(t *testing.T, script string) string {
	t.Helper()
	out, _ := exec.Command("sh", "-c", script+"\nprintf '%s\\n' \"$members\"").CombinedOutput()
	return strings.TrimSpace(string(out))
}

// A process that exits between the shell expanding /proc/[0-9]*/stat and awk
// opening each file leaves an unreadable path in awk's argument list. mawk —
// the default awk on Debian/Ubuntu, so on the CI runner and most dev images —
// aborts the entire scan there instead of skipping that one file, so every
// member after the vanished pid goes unseen.
//
// The consequence is not a cosmetic one: an empty member list makes
// `okdev jobs stop` and the --kill-group-on-exit cascade fall back to
// signaling the job leader alone, leaving the process group (torchrun ranks
// holding GPU memory) running while the command reports success. A busy
// training pod is exactly where a pid is most likely to vanish mid-scan.
//
// BSD awk on macOS skips the missing file and continues, which is why this
// only ever showed up in CI.
func TestProcGroupMembersScriptSurvivesVanishedPid(t *testing.T) {
	root := t.TempDir()
	writeFakeProcStat(t, root, "101", "sh", "S", "1", "500")
	writeFakeProcStat(t, root, "102", "sleep", "S", "101", "500")
	writeFakeProcStat(t, root, "103", "sleep", "S", "101", "500")

	// The middle entry is the pid that exited after the glob was expanded.
	list := strings.Join([]string{
		filepath.Join(root, "101", "stat"),
		filepath.Join(root, "999999", "stat"),
		filepath.Join(root, "102", "stat"),
		filepath.Join(root, "103", "stat"),
	}, " ")

	got := runScanScript(t, "pgid=500\n"+procGroupMembersScript(list))
	members := strings.Fields(got)
	if len(members) != 3 {
		t.Fatalf("a vanished pid must not truncate the scan: got %v, want all of 101 102 103", members)
	}
	for _, want := range []string{"101", "102", "103"} {
		if !strings.Contains(got, want) {
			t.Fatalf("missing member %s in %q", want, got)
		}
	}
}

func TestProcGroupMembersScriptSelectsOnlyTheGroup(t *testing.T) {
	root := t.TempDir()
	writeFakeProcStat(t, root, "101", "sh", "S", "1", "500")     // in group
	writeFakeProcStat(t, root, "104", "other", "S", "1", "600")  // different group
	writeFakeProcStat(t, root, "105", "gone", "Z", "101", "500") // zombie in group
	// A comm containing spaces and parens must not shift the field offsets.
	writeFakeProcStat(t, root, "106", "my proc (x)", "S", "101", "500")

	list := strings.Join([]string{
		filepath.Join(root, "101", "stat"),
		filepath.Join(root, "104", "stat"),
		filepath.Join(root, "105", "stat"),
		filepath.Join(root, "106", "stat"),
	}, " ")

	got := strings.Fields(runScanScript(t, "pgid=500\n"+procGroupMembersScript(list)))
	want := map[string]bool{"101": true, "106": true}
	if len(got) != len(want) {
		t.Fatalf("got %v, want exactly the two live group members", got)
	}
	for _, pid := range got {
		if !want[pid] {
			t.Fatalf("unexpected member %s in %v (zombies and other groups must be excluded)", pid, got)
		}
	}
}
