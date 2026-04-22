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
	var exitErr utilexec.ExitError
	if errors.As(err, &exitErr) && exitErr.Exited() {
		return exitErr.ExitStatus()
	}
	return 1
}
