package cli

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestFormatMeshSummary(t *testing.T) {
	if got := formatMeshSummary(nil); got != "no receivers" {
		t.Fatalf("expected 'no receivers', got %q", got)
	}

	summary := &meshSummary{Receivers: []meshReceiverStatus{}}
	if got := formatMeshSummary(summary); got != "0 receiver(s) connected and synced" {
		t.Fatalf("expected 0 connected, got %q", got)
	}

	summary = &meshSummary{Receivers: []meshReceiverStatus{
		{Pod: "recv-1", Connected: true, Synced: true},
		{Pod: "recv-2", Connected: true, Synced: true},
	}}
	if got := formatMeshSummary(summary); got != "2 receiver(s) connected and synced" {
		t.Fatalf("expected 2 connected, got %q", got)
	}

	summary = &meshSummary{Receivers: []meshReceiverStatus{
		{Pod: "recv-1", Connected: true, Synced: true},
		{Pod: "recv-2", Err: errors.New("timeout")},
	}}
	if got := formatMeshSummary(summary); got != "1/2 receiver(s) synced, 1 failed" {
		t.Fatalf("expected failed summary, got %q", got)
	}
}

func TestMeshStatusLines(t *testing.T) {
	if got := meshStatusLines(nil); len(got) != 0 {
		t.Fatalf("expected empty lines for nil summary, got %v", got)
	}

	summary := &meshSummary{HubPod: "hub", Receivers: []meshReceiverStatus{
		{Pod: "recv-1", Connected: true, Synced: true},
		{Pod: "recv-2", Err: errors.New("timeout")},
	}}
	lines := meshStatusLines(summary)

	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "topology: hub-and-spoke") {
		t.Fatalf("expected topology, got %q", joined)
	}
	if !strings.Contains(joined, "hub: hub") {
		t.Fatalf("expected hub name, got %q", joined)
	}
	if !strings.Contains(joined, "receivers: 1/2 connected") {
		t.Fatalf("expected receiver summary, got %q", joined)
	}
	if !strings.Contains(joined, "recv-1: synced") {
		t.Fatalf("expected recv-1 status, got %q", joined)
	}
	if !strings.Contains(joined, "recv-2: error: timeout") {
		t.Fatalf("expected recv-2 error, got %q", joined)
	}
}

func TestMeshReceiverCount(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Logf("Got request: %s %s", r.Method, r.URL.String())
		if r.URL.Path == "/api/v1/namespaces/demo/pods" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[
				{"metadata":{"name":"pod-1"}},
				{"metadata":{"name":"pod-2"}}
			]}`)
			return
		}
		http.NotFound(w, r)
	}))
	defer server.Close()

	kubeconfig := writeCLITLSTestKubeconfig(t, server)
	t.Setenv("KUBECONFIG", kubeconfig)

	client := &kube.Client{}

	count, err := meshReceiverCount(context.Background(), client, "demo", map[string]string{"okdev.io/session": "foo"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 2 {
		t.Fatalf("expected 2 receivers, got %d", count)
	}
}
