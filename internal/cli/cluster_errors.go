package cli

import (
	"context"
	"errors"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// Exit codes used to let scripted / agent callers tell apart a transient
// cluster-contact hiccup (worth retrying) from a session that is genuinely
// gone (a real state change worth acting on). They follow the spirit of
// sysexits.h and the okdev-exec-output spec §2.
const (
	// ExitTransientCluster signals that okdev could not reach the cluster
	// API in a way that is likely to clear on its own (timeout, connection
	// reset, 503). Callers should retry rather than treat the session as
	// dead.
	ExitTransientCluster = 78
	// ExitSessionNotFound signals that the cluster was reachable but the
	// requested session has no pods — a real "it's gone" condition.
	ExitSessionNotFound = 74
	// ExitExecInfraFailure signals that a fanout exec could not run the
	// command on at least one pod (unreachable, timeout, container gone) —
	// as opposed to the command itself exiting non-zero, which stays exit
	// 1. Follows sysexits EX_UNAVAILABLE.
	ExitExecInfraFailure = 69
)

// ErrSessionNotFound is returned when session resolution reaches the cluster
// successfully but finds no pods for the session. Wrap with %w so callers and
// the top-level exit-code classifier can detect it via errors.Is.
var ErrSessionNotFound = errors.New("session not found")

// ErrTransientCluster wraps a recoverable cluster-contact failure (API
// timeout, connection refused/reset, server overload). It is distinct from a
// session that no longer exists.
var ErrTransientCluster = errors.New("transient cluster contact failure")

// ErrExecInfraFailure marks a fanout exec where at least one pod could not
// run the command at all (transport, timeout, container gone). Wrapped into
// the returned error so the top-level classifier maps it to
// ExitExecInfraFailure, letting scripts tell real delivery failures apart
// from a command that simply exited non-zero (plain exit 1).
var ErrExecInfraFailure = errors.New("exec delivery failure")

// ClassifiedExitCode maps okdev's sentinel cluster errors to their dedicated
// exit codes. It returns ok=false for anything it does not recognize so the
// caller can fall back to its default (e.g. a remote command's own exit code,
// or 1). It deliberately does NOT classify command-result errors such as a
// failed job or a non-zero remote command — those stay exit 1.
func ClassifiedExitCode(err error) (int, bool) {
	switch {
	case errors.Is(err, ErrSessionNotFound):
		return ExitSessionNotFound, true
	case errors.Is(err, ErrTransientCluster):
		return ExitTransientCluster, true
	case errors.Is(err, ErrExecInfraFailure):
		return ExitExecInfraFailure, true
	default:
		return 0, false
	}
}

// isTransientClusterError reports whether err looks like a recoverable
// cluster-contact failure rather than a permanent one (RBAC denial, bad
// request) or a logical "not found". Permanent failures must NOT be retried
// or classified as transient, so they are checked first.
func isTransientClusterError(err error) bool {
	if err == nil {
		return false
	}
	// Permanent API conditions: never transient.
	if apierrors.IsForbidden(err) || apierrors.IsUnauthorized(err) ||
		apierrors.IsNotFound(err) || apierrors.IsBadRequest(err) ||
		apierrors.IsInvalid(err) {
		return false
	}
	// Structured transient API conditions.
	if apierrors.IsTimeout(err) || apierrors.IsServerTimeout(err) ||
		apierrors.IsServiceUnavailable(err) || apierrors.IsTooManyRequests(err) ||
		apierrors.IsInternalError(err) {
		return true
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	// Network-level failures that the typed checks above miss often only
	// surface as message text from the transport layer.
	msg := strings.ToLower(err.Error())
	transientSubstrings := []string{
		"context deadline exceeded",
		"connection refused",
		"connection reset",
		"i/o timeout",
		"timeout",
		"no route to host",
		"tls handshake timeout",
		"unexpected eof",
		"server is currently unable to handle the request",
		"etcdserver: request timed out",
	}
	for _, s := range transientSubstrings {
		if strings.Contains(msg, s) {
			return true
		}
	}
	return false
}
