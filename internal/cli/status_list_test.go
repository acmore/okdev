package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/session"
)

func TestNewStatusCmdOutputsJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all", "--all-users"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0]["session"] != "sess-a" || rows[0]["phase"] != "Running" {
		t.Fatalf("unexpected status rows: %#v", rows)
	}
}

func TestNewStatusCmdFallsBackToSavedWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs/trainer":
			_, _ = io.WriteString(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"namespace":"demo","name":"trainer"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	if err := session.SaveInfo(session.Info{
		Name:               "sess-a",
		Namespace:          "demo",
		Owner:              "alice",
		WorkloadType:       "job",
		WorkloadAPIVersion: "batch/v1",
		WorkloadKind:       "Job",
		WorkloadName:       "trainer",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Session: "sess-a", Owner: "alice"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 fallback status row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected fallback row: %#v", rows[0])
	}
}

func TestNewStatusCmdUsesActiveControllerBackedSessionWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			http.NotFound(w, r)
		case "/apis/batch/v1/namespaces/demo/jobs/trainer":
			_, _ = io.WriteString(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"namespace":"demo","name":"trainer"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	if err := session.SaveActiveSession("sess-a"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}
	if err := session.SaveInfo(session.Info{
		Name:               "sess-a",
		Namespace:          "demo",
		Owner:              "alice",
		WorkloadType:       "job",
		WorkloadAPIVersion: "batch/v1",
		WorkloadKind:       "Job",
		WorkloadName:       "trainer",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Owner: "alice"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 status row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected active-session fallback row: %#v", rows[0])
	}
}

func TestNewStatusCmdDiscoversLiveJobWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs":
			_, _ = io.WriteString(w, `{"kind":"JobList","apiVersion":"batch/v1","items":[{"metadata":{"namespace":"demo","name":"trainer","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"job"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Session: "sess-a", Owner: "alice"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 live controller row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected live controller row: %#v", rows[0])
	}
}

func TestNewStatusCmdAllIncludesSavedPendingWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs/trainer":
			_, _ = io.WriteString(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"namespace":"demo","name":"trainer"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	if err := session.SaveInfo(session.Info{
		Name:               "sess-a",
		Namespace:          "demo",
		Owner:              "alice",
		WorkloadType:       "job",
		WorkloadAPIVersion: "batch/v1",
		WorkloadKind:       "Job",
		WorkloadName:       "trainer",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Owner: "alice"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 pending status row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected pending status row: %#v", rows[0])
	}
}

func TestNewStatusCmdAllIncludesLivePendingControllerWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs":
			_, _ = io.WriteString(w, `{"kind":"JobList","apiVersion":"batch/v1","items":[{"metadata":{"namespace":"demo","name":"trainer","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"job"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Owner: "alice"}
	cmd := newStatusCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("status execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 live pending status row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected live pending status row: %#v", rows[0])
	}
}

func TestNewListCmdOutputsJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	opts := &Options{Namespace: "demo", Context: "dev", Output: "json"}
	cmd := newListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all-users"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("list execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0]["session"] != "sess-a" || rows[0]["namespace"] != "demo" {
		t.Fatalf("unexpected list rows: %#v", rows)
	}
}

func TestNewListCmdFallsBackToSavedWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs/trainer":
			_, _ = io.WriteString(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"namespace":"demo","name":"trainer"}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	if err := session.SaveInfo(session.Info{
		Name:               "sess-a",
		Namespace:          "demo",
		Owner:              "alice",
		WorkloadType:       "job",
		WorkloadAPIVersion: "batch/v1",
		WorkloadKind:       "Job",
		WorkloadName:       "trainer",
	}); err != nil {
		t.Fatalf("SaveInfo: %v", err)
	}

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	opts := &Options{Namespace: "demo", Context: "dev", Output: "json", Owner: "alice"}
	cmd := newListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("list execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 fallback list row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected fallback list row: %#v", rows[0])
	}
}

func TestNewListCmdDiscoversLiveJobWorkloadWhenNoPodsExistYet(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case "/apis/batch/v1/namespaces/demo/jobs":
			_, _ = io.WriteString(w, `{"kind":"JobList","apiVersion":"batch/v1","items":[{"metadata":{"namespace":"demo","name":"trainer","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"job"}}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	opts := &Options{Namespace: "demo", Context: "dev", Output: "json", Owner: "alice"}
	cmd := newListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("list execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 {
		t.Fatalf("expected 1 live controller row, got %#v", rows)
	}
	if rows[0]["session"] != "sess-a" || rows[0]["workload"] != "job" || rows[0]["phase"] != "Pending" {
		t.Fatalf("unexpected live controller list row: %#v", rows[0])
	}
}

func TestNewListCmdDefaultsToAllNamespacesForCurrentOwner(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"proj-tango","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		case "/api/v1/namespaces/default/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "default")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Output: "json", Owner: "alice"}
	cmd := newListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("list execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0]["session"] != "sess-a" || rows[0]["namespace"] != "proj-tango" {
		t.Fatalf("unexpected list rows: %#v", rows)
	}
}

func TestNewListCmdNamespaceOverrideNarrowsResults(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		case "/api/v1/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"proj-tango","name":"okdev-sess-b","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-b","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	opts := &Options{Namespace: "demo", Context: "dev", Output: "json", Owner: "alice"}
	cmd := newListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("list execute: %v", err)
	}

	var rows []map[string]any
	if err := json.Unmarshal(out.Bytes(), &rows); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if len(rows) != 1 || rows[0]["session"] != "sess-a" || rows[0]["namespace"] != "demo" {
		t.Fatalf("unexpected list rows: %#v", rows)
	}
}

func TestNewStatusCmdDetailsRequiresSingleSession(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[
{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}},
{"metadata":{"namespace":"demo","name":"okdev-sess-b","creationTimestamp":"2026-03-29T00:01:00Z","labels":{"okdev.io/session":"sess-b","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}
]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev"}
	cmd := newStatusCmd(opts)
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--all", "--all-users", "--details"})

	err := cmd.Execute()
	if err == nil || err.Error() != "--details requires a single session" {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewAgentListCmdReportsNoConfiguredAgents(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"okdev-sess-a"},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice"}
	cmd := newAgentListCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("agent list execute: %v", err)
	}
	if got := out.String(); got != "No agents configured. Add spec.agents to .okdev.yaml to enable agent support.\n" {
		t.Fatalf("unexpected output %q", got)
	}
}

func writeCLIConfig(t *testing.T, namespace string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), ".okdev.yaml")
	content := fmt.Sprintf(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: %s
`, namespace)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeCLITLSTestKubeconfig(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	path := filepath.Join(t.TempDir(), "config")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: dev
contexts:
- context:
    cluster: dev
    user: dev
  name: dev
current-context: dev
users:
- name: dev
  user:
    token: test
`, (&url.URL{Scheme: u.Scheme, Host: u.Host}).String())
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
