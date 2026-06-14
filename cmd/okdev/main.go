package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/acmore/okdev/internal/cli"
	utilexec "k8s.io/utils/exec"
)

func main() {
	if err := cli.Execute(); err != nil {
		if shouldPrintError(err) {
			fmt.Fprintln(os.Stderr, err)
		}
		os.Exit(exitCode(err))
	}
}

func shouldPrintError(err error) bool {
	return !errors.Is(err, cli.ErrProxyHealthDisconnect)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	// A remote command's own exit status wins over everything: `okdev exec
	// -- false` must exit 1, `exit 7` must exit 7, regardless of any cluster
	// classification. This also keeps jobs/command-result failures intact.
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return exitErr.ExitStatus()
	}
	// Otherwise distinguish "cluster unreachable, retry" (78) from "session
	// gone" (74) so monitoring callers don't conflate the two.
	if code, ok := cli.ClassifiedExitCode(err); ok {
		return code
	}
	return 1
}
