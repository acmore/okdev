package kube

import (
	"errors"
	"fmt"
	"net/url"
	"testing"

	"k8s.io/apimachinery/pkg/util/httpstream"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/remotecommand"
)

func TestNewExecExecutorPrefersWebSocketWithSPDYFallback(t *testing.T) {
	cfg := &rest.Config{Host: "https://example.invalid"}
	u, err := url.Parse("https://example.invalid/api/v1/namespaces/default/pods/p/exec")
	if err != nil {
		t.Fatal(err)
	}
	exec, err := newExecExecutor(cfg, u)
	if err != nil {
		t.Fatalf("newExecExecutor: %v", err)
	}
	if _, ok := exec.(*remotecommand.FallbackExecutor); !ok {
		t.Fatalf("expected a FallbackExecutor, got %T", exec)
	}
}

func TestIsExecFallbackError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil", err: nil, want: false},
		{
			name: "upgrade failure",
			err:  &httpstream.UpgradeFailureError{Cause: errors.New("websockets not supported")},
			want: true,
		},
		{
			name: "wrapped upgrade failure",
			err:  fmt.Errorf("exec: %w", &httpstream.UpgradeFailureError{Cause: errors.New("nope")}),
			want: true,
		},
		{
			name: "https proxy error",
			err:  errors.New("proxy: unknown scheme: https"),
			want: true,
		},
		{
			// A remote command's own failure must never trigger fallback, or
			// the command could run twice.
			name: "remote exit",
			err:  errors.New("command terminated with exit code 1"),
			want: false,
		},
		{name: "stream error", err: errors.New("error reading from error stream: EOF"), want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isExecFallbackError(tc.err); got != tc.want {
				t.Fatalf("isExecFallbackError(%v) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}
