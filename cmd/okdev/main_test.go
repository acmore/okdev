package main

import (
	"errors"
	"testing"

	"github.com/acmore/okdev/internal/cli"
	utilexec "k8s.io/utils/exec"
)

func TestExitCodeUsesRemoteExecExitCode(t *testing.T) {
	err := utilexec.CodeExitError{
		Err:  errors.New("command terminated with exit code 137"),
		Code: 137,
	}

	if got := exitCode(err); got != 137 {
		t.Fatalf("expected exit code 137, got %d", got)
	}
}

func TestExitCodeUsesOneForGenericErrors(t *testing.T) {
	if got := exitCode(errors.New("boom")); got != 1 {
		t.Fatalf("expected exit code 1, got %d", got)
	}
}

func TestShouldPrintErrorSuppressesProxyDisconnect(t *testing.T) {
	if shouldPrintError(cli.ErrProxyHealthDisconnect) {
		t.Fatal("expected proxy disconnect error to be suppressed")
	}
}

func TestShouldPrintErrorPrintsOtherErrors(t *testing.T) {
	if !shouldPrintError(errors.New("boom")) {
		t.Fatal("expected generic error to be printed")
	}
}
