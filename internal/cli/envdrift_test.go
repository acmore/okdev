package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestInstallCommandHint(t *testing.T) {
	for _, cmd := range []string{
		"sh -lc 'apt-get install -y libaio1'",
		"pip install deepspeed",
		"bash -c 'pip3 install -e .'",
		"conda install pytorch",
		"apk add openssh",
	} {
		if installCommandHint(cmd) == "" {
			t.Fatalf("expected hint for %q", cmd)
		}
	}
	for _, cmd := range []string{
		"python train.py",
		"pip freeze",
		"apt-get update",        // update alone mutates only caches
		"echo pip installed ok", // not an install verb
		"nvidia-smi",
	} {
		if hint := installCommandHint(cmd); hint != "" {
			t.Fatalf("no hint expected for %q, got %q", cmd, hint)
		}
	}
}

func TestDiffPackageInventoriesAndDraft(t *testing.T) {
	baseline := parsePackageInventory("os:curl=7.81.0\nos:vim=8.2\npy:torch==2.3.0\n")
	current := parsePackageInventory("os:curl=7.81.0\nos:libaio1=0.3.113\npy:torch==2.4.0\npy:deepspeed==0.14.0\n")
	drift := diffPackageInventories(baseline, current)
	if len(drift.Added) != 2 || drift.Added[0] != "os:libaio1=0.3.113" || drift.Added[1] != "py:deepspeed==0.14.0" {
		t.Fatalf("added = %v", drift.Added)
	}
	if len(drift.Removed) != 1 || drift.Removed[0] != "os:vim=8.2" {
		t.Fatalf("removed = %v", drift.Removed)
	}
	if len(drift.Changed) != 1 || !strings.Contains(drift.Changed[0], "torch==2.4.0 (was py:torch==2.3.0)") {
		t.Fatalf("changed = %v", drift.Changed)
	}
	draft := drift.postCreateDraft()
	for _, want := range []string{"postCreate", "apt-get install -y libaio1=0.3.113", "pip install deepspeed==0.14.0"} {
		if !strings.Contains(draft, want) {
			t.Fatalf("draft missing %q:\n%s", want, draft)
		}
	}

	if !diffPackageInventories(baseline, baseline).empty() {
		t.Fatal("identical inventories must produce no drift")
	}
}

// The collector and baseline scripts must run on a plain shell: verify the
// end-to-end capture -> mutate -> diff flow against local fake package
// managers on PATH.
func TestEnvCollectorScriptsThroughRealShell(t *testing.T) {
	dir := t.TempDir()
	state := filepath.Join(dir, "pipstate")
	os.WriteFile(state, []byte("torch==2.3.0\n"), 0o644)
	fakePip := "#!/bin/sh\n[ \"$1\" = freeze ] && cat " + state + "\n"
	if err := os.WriteFile(filepath.Join(dir, "pip"), []byte(fakePip), 0o755); err != nil {
		t.Fatal(err)
	}

	runIn := func(script string) (string, int) {
		cmd := exec.Command("sh", "-c", script)
		cmd.Env = []string{"PATH=" + dir + ":/usr/bin:/bin", "TMPDIR=" + dir}
		var out, errBuf bytes.Buffer
		cmd.Stdout = &out
		cmd.Stderr = &errBuf
		_ = cmd.Run()
		return out.String(), cmd.ProcessState.ExitCode()
	}

	// No baseline yet: the read script fails distinctly.
	baselineDirLocal := filepath.Join(dir, "baseline")
	readScript := strings.ReplaceAll(envBaselineReadScript, envBaselineDir, baselineDirLocal)
	if _, code := runIn(readScript); code != 21 {
		t.Fatalf("missing baseline should exit 21, got %d", code)
	}

	captureScript := strings.ReplaceAll(envBaselineCaptureScript(), envBaselineDir, baselineDirLocal)
	if _, code := runIn(captureScript); code != 0 {
		t.Fatalf("baseline capture failed: %d", code)
	}
	baselineOut, _ := runIn(readScript)
	if !strings.Contains(baselineOut, "py:torch==2.3.0") {
		t.Fatalf("baseline missing torch: %q", baselineOut)
	}

	// Manual drift: a new package appears.
	os.WriteFile(state, []byte("torch==2.3.0\ndeepspeed==0.14.0\n"), 0o644)
	currentOut, _ := runIn(envCollectorScript)
	drift := diffPackageInventories(parsePackageInventory(baselineOut), parsePackageInventory(currentOut))
	if len(drift.Added) != 1 || drift.Added[0] != "py:deepspeed==0.14.0" {
		t.Fatalf("expected deepspeed drift, got %+v", drift)
	}
}
