package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeNvidiaSMI puts an nvidia-smi on PATH that reports two idle GPUs.
func fakeNvidiaSMI(t *testing.T) {
	t.Helper()
	bin := t.TempDir()
	script := `#!/bin/sh
case "$*" in
  *--query-gpu=index,uuid*) printf '0, GPU-a\n1, GPU-b\n' ;;
esac
`
	if err := os.WriteFile(filepath.Join(bin, "nvidia-smi"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
}

// The subcommand replaces the process, so the tests run it through the test
// binary itself.
func TestMain(m *testing.M) {
	if os.Getenv("OKDEV_TEST_RUN_GPUS") == "1" {
		os.Exit(runGPUs(os.Args[1:], os.Stderr))
	}
	os.Exit(m.Run())
}

func runGPUsProcess(t *testing.T, args ...string) (string, string, int) {
	t.Helper()
	cmd := exec.Command(os.Args[0], args...)
	cmd.Env = append(os.Environ(), "OKDEV_TEST_RUN_GPUS=1", "OKDEV_WORKTREE_ID=wt-a")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	code := 0
	if exitErr, ok := err.(*exec.ExitError); ok {
		code = exitErr.ExitCode()
	} else if err != nil {
		t.Fatal(err)
	}
	return stdout.String(), stderr.String(), code
}

func TestGPUsRunLeasesThenBecomesTheCommand(t *testing.T) {
	fakeNvidiaSMI(t)
	dir := t.TempDir()

	if _, _, code := runGPUsProcess(t, "--probe"); code != 0 {
		t.Fatalf("probe exit = %d", code)
	}

	// The command sees only its GPUs, and holds them for as long as it runs:
	// a second request made from inside it cannot get both.
	inner := os.Args[0] + ` --dir "$LEASES" --count 2 -- true; echo "inner=$?"`
	t.Setenv("LEASES", dir)
	stdout, stderr, code := runGPUsProcess(t, "run", "--dir", dir, "--count", "1", "--",
		"sh", "-c", `echo "devices=$CUDA_VISIBLE_DEVICES gpus=$OKDEV_GPUS"; `+inner)
	if code != 0 {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
	if !strings.Contains(stdout, "devices=GPU-a gpus=0") {
		t.Fatalf("stdout = %q", stdout)
	}
	if !strings.Contains(stdout, "inner=75") || !strings.Contains(stderr, "gpu 0: worktree wt-a") {
		t.Fatalf("lease not held by the running command: stdout=%q stderr=%q", stdout, stderr)
	}

	// Once the command is gone, so is its lease; its exit code is its own.
	stdout, stderr, code = runGPUsProcess(t, "run", "--dir", dir, "--count", "all", "--", "sh", "-c", `echo "$OKDEV_GPUS"; exit 7`)
	if code != 7 || strings.TrimSpace(stdout) != "0,1" {
		t.Fatalf("exit = %d, stdout = %q, stderr = %s", code, stdout, stderr)
	}

	if _, stderr, code := runGPUsProcess(t, "run", "--dir", dir, "--count", "3", "--", "true"); code != gpusExitUnavailable || !strings.Contains(stderr, "3 GPUs requested but this container has 2") {
		t.Fatalf("oversized request: exit = %d, stderr = %s", code, stderr)
	}
	if _, _, code := runGPUsProcess(t, "run", "--dir", dir, "--count", "0", "--", "true"); code != 2 {
		t.Fatalf("invalid count exit = %d", code)
	}
}

func TestGPUsRunWithoutNvidiaSMI(t *testing.T) {
	bin := t.TempDir()
	for _, tool := range []string{"true", "sh"} {
		path, err := exec.LookPath(tool)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(path, filepath.Join(bin, tool)); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("PATH", bin)
	_, stderr, code := runGPUsProcess(t, "run", "--dir", t.TempDir(), "--count", "1", "--", "true")
	if code != gpusExitUnavailable || !strings.Contains(stderr, "nvidia-smi not found") {
		t.Fatalf("exit = %d, stderr = %s", code, stderr)
	}
}
