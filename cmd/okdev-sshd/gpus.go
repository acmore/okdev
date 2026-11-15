package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	flag "github.com/spf13/pflag"

	"github.com/acmore/okdev/internal/gpulease"
)

// Exit codes of `okdev-sshd gpus run` for failures before the command starts,
// taken from sysexits so they are unlikely to be mistaken for the command's
// own: the request can never be met here, or it could not be met right now.
const (
	gpusExitUnavailable = 69 // EX_UNAVAILABLE
	gpusExitBusy        = 75 // EX_TEMPFAIL
)

const (
	gpusLeaseDir         = "/var/okdev/gpus"
	gpusFallbackLeaseDir = "/tmp/okdev-gpus"
)

// runGPUs implements `okdev-sshd gpus`, the in-container half of
// `okdev exec --gpus`: lease GPUs, then become the command. The leases ride
// along as open file descriptors, so they end with the command's process
// tree and need no wrapper process to stay around.
func runGPUs(args []string, stderr io.Writer) int {
	fs := flag.NewFlagSet("gpus", flag.ContinueOnError)
	fs.SetOutput(stderr)
	probe := fs.Bool("probe", false, "Exit 0: this binary supports the gpus subcommand")
	count := fs.String("count", "", "Number of GPUs to lease, or \"all\"")
	wait := fs.Duration("wait", 0, "How long to wait for GPUs to become free")
	dir := fs.String("dir", "", "Lease directory (default "+gpusLeaseDir+")")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *probe {
		return 0
	}
	command := fs.Args()
	if len(command) > 0 && command[0] == "run" {
		command = command[1:]
	}
	want, err := parseGPUCount(*count)
	if err != nil || len(command) == 0 {
		fmt.Fprintln(stderr, "usage: okdev-sshd gpus run --count <n|all> [--wait <duration>] -- <command>...")
		return 2
	}
	binary, err := exec.LookPath(command[0])
	if err != nil {
		fmt.Fprintf(stderr, "okdev: %v\n", err)
		return 127
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	holder := gpulease.Holder{
		Worktree: os.Getenv("OKDEV_WORKTREE_ID"),
		JobID:    os.Getenv("OKDEV_JOB_ID"),
		PID:      os.Getpid(),
		Command:  truncateCommand(strings.Join(command, " "), 200),
		Since:    time.Now().UTC(),
	}
	if *wait > 0 {
		fmt.Fprintf(stderr, "okdev: waiting up to %s for %s GPU(s)\n", *wait, *count)
	}
	lease, err := gpulease.Acquire(ctx, resolveGPULeaseDir(*dir), want, *wait, holder, gpulease.NvidiaSMI{})
	if err != nil {
		var unavailable *gpulease.UnavailableError
		if errors.As(err, &unavailable) {
			fmt.Fprintf(stderr, "okdev: gpus busy: %v\n", err)
			return gpusExitBusy
		}
		fmt.Fprintf(stderr, "okdev: gpus unavailable: %v\n", err)
		return gpusExitUnavailable
	}
	for _, f := range lease.Files() {
		// Go opens files close-on-exec; the lease must survive the exec.
		if _, _, errno := syscall.Syscall(syscall.SYS_FCNTL, f.Fd(), syscall.F_SETFD, 0); errno != 0 {
			fmt.Fprintf(stderr, "okdev: keep GPU lease across exec: %v\n", errno)
			return 1
		}
	}
	fmt.Fprintf(stderr, "okdev: leased gpu %s\n", lease.Indices())
	env := gpuCommandEnv(os.Environ(), lease.VisibleDevices(), lease.Indices())
	stop()
	if err := syscall.Exec(binary, command, env); err != nil {
		fmt.Fprintf(stderr, "okdev: exec %s: %v\n", binary, err)
		return 126
	}
	return 0
}

func gpuCommandEnv(base []string, visible, indices string) []string {
	env := make([]string, 0, len(base)+2)
	for _, entry := range base {
		if strings.HasPrefix(entry, "CUDA_VISIBLE_DEVICES=") || strings.HasPrefix(entry, "OKDEV_GPUS=") {
			continue
		}
		env = append(env, entry)
	}
	return append(env, "CUDA_VISIBLE_DEVICES="+visible, "OKDEV_GPUS="+indices)
}

func parseGPUCount(count string) (int, error) {
	if count == "all" {
		return 0, nil
	}
	n, err := strconv.Atoi(count)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("invalid GPU count %q", count)
	}
	return n, nil
}

// resolveGPULeaseDir prefers the pod's shared runtime volume, where every
// container user finds the same leases, and falls back to /tmp when that
// volume is not writable for this user (the same rule detached jobs use).
func resolveGPULeaseDir(dir string) string {
	if dir != "" {
		return dir
	}
	if err := os.MkdirAll(gpusLeaseDir, 0o777); err == nil {
		if f, err := os.CreateTemp(gpusLeaseDir, ".probe-*"); err == nil {
			_ = f.Close()
			_ = os.Remove(f.Name())
			return gpusLeaseDir
		}
	}
	return gpusFallbackLeaseDir
}

func truncateCommand(command string, limit int) string {
	if len(command) <= limit {
		return command
	}
	return command[:limit] + "…"
}
