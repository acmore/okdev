package kube

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

func TestReadinessWatchReturnsErrorsForCallerRecovery(t *testing.T) {
	for _, tc := range []struct {
		name, event string
		check       func(error) bool
	}{
		{"forbidden", `{"type":"ERROR","object":{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Forbidden","message":"watch denied","code":403}}`, apierrors.IsForbidden},
		{"expired", `{"type":"ERROR","object":{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Expired","message":"old resource version","code":410}}`, apierrors.IsResourceExpired},
		{"closed stream", "", func(err error) bool { return errors.Is(err, io.ErrUnexpectedEOF) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var watches atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Query().Get("watch") == "true" {
					watches.Add(1)
					w.WriteHeader(http.StatusOK)
					if tc.event != "" {
						_, _ = io.WriteString(w, tc.event+"\n")
					}
					w.(http.Flusher).Flush()
					return
				}
				_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"name":"target","namespace":"default","resourceVersion":"1"},"status":{"phase":"Pending"}}`)
			}))
			defer server.Close()
			t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))
			err := (&Client{Context: "dev"}).WaitReadyWithProgress(context.Background(), "default", "target", 250*time.Millisecond, nil)
			if !tc.check(err) {
				t.Fatalf("error = %v, want %s", err, tc.name)
			}
			if watches.Load() != 1 {
				t.Fatalf("unexpected watch retry loop: %d", watches.Load())
			}
		})
	}
}
