package cli

import (
	"context"
	"errors"
	"fmt"
	"testing"
)

func TestIsTransientClusterError(t *testing.T) {
	transient := []error{
		context.DeadlineExceeded,
		errors.New("Get \"https://10.0.0.1:6443/api/v1/...\": context deadline exceeded"),
		errors.New("dial tcp 10.0.0.1:6443: connect: connection refused"),
		errors.New("read tcp: connection reset by peer"),
		errors.New("net/http: TLS handshake timeout"),
		errors.New("etcdserver: request timed out"),
	}
	for _, err := range transient {
		if !isTransientClusterError(err) {
			t.Fatalf("expected transient: %v", err)
		}
	}

	permanent := []error{
		nil,
		errors.New("pods is forbidden: User cannot list resource"),
		errors.New("the server could not find the requested resource"),
		errors.New("admission webhook denied the request"),
	}
	for _, err := range permanent {
		if isTransientClusterError(err) {
			t.Fatalf("expected non-transient: %v", err)
		}
	}
}

func TestClassifiedExitCode(t *testing.T) {
	if code, ok := ClassifiedExitCode(fmt.Errorf("wrap: %w", ErrSessionNotFound)); !ok || code != ExitSessionNotFound {
		t.Fatalf("not-found: got code=%d ok=%v", code, ok)
	}
	if code, ok := ClassifiedExitCode(fmt.Errorf("wrap: %w", ErrTransientCluster)); !ok || code != ExitTransientCluster {
		t.Fatalf("transient: got code=%d ok=%v", code, ok)
	}
	if _, ok := ClassifiedExitCode(errors.New("some remote command failed")); ok {
		t.Fatal("unclassified error should return ok=false")
	}
	if _, ok := ClassifiedExitCode(nil); ok {
		t.Fatal("nil should return ok=false")
	}
}
