package main

import (
	"errors"
	"fmt"
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

func TestExitCodeClassifiesClusterErrors(t *testing.T) {
	if got := exitCode(fmt.Errorf("wrap: %w", cli.ErrSessionNotFound)); got != cli.ExitSessionNotFound {
		t.Fatalf("expected session-not-found exit %d, got %d", cli.ExitSessionNotFound, got)
	}
	if got := exitCode(fmt.Errorf("wrap: %w", cli.ErrTransientCluster)); got != cli.ExitTransientCluster {
		t.Fatalf("expected transient exit %d, got %d", cli.ExitTransientCluster, got)
	}
}

func TestExitCodeRemoteExitWinsOverClusterClassification(t *testing.T) {
	// A remote command's own non-zero exit must not be masked by cluster
	// classification, even if a classified error is somewhere in the chain.
	err := fmt.Errorf("%w: while contacting cluster", cli.ErrTransientCluster)
	wrapped := utilexec.CodeExitError{Err: err, Code: 7}
	if got := exitCode(wrapped); got != 7 {
		t.Fatalf("expected remote exit 7 to win, got %d", got)
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
