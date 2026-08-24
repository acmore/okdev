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
	"sync/atomic"
	"testing"

	"github.com/acmore/okdev/internal/session"
	"github.com/spf13/cobra"
)

// The commands below take no --context: the only thing pointing them at a
// cluster is spec.kubeContext. The kubeconfig's current-context is wired to a
// second, empty cluster that looks perfectly healthy, so honoring the pin is
// the only way to find the session — and ignoring it fails as silently in the
// test as it did in the field.

func TestListHonorsPinnedKubeContextWithNamespaceFlag(t *testing.T) {
	pinned, current, currentHits := startPinnedAndCurrentClusters(t)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLIPinnedContextKubeconfig(t, pinned, current))
	cfgPath := writeCLIConfigWithKubeContext(t, "demo", "pinned")

	// --namespace is a persistent root flag; it lands here.
	opts := &Options{ConfigPath: cfgPath, Namespace: "demo", Output: "json", Owner: "alice"}
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
	if len(rows) != 1 || rows[0]["session"] != "sess-a" {
		t.Fatalf("unexpected list rows: %#v", rows)
	}
	if hits := currentHits.Load(); hits != 0 {
		t.Fatalf("--namespace made list fall back to the current-context cluster (%d requests)", hits)
	}
}

func TestStatusHonorsPinnedKubeContext(t *testing.T) {
	pinned, current, currentHits := startPinnedAndCurrentClusters(t)

	t.Setenv("HOME", t.TempDir())
	if err := session.SaveActiveSession("sess-a"); err != nil {
		t.Fatalf("SaveActiveSession: %v", err)
	}
	t.Setenv("KUBECONFIG", writeCLIPinnedContextKubeconfig(t, pinned, current))
	cfgPath := writeCLIConfigWithKubeContext(t, "demo", "pinned")

	opts := &Options{ConfigPath: cfgPath, Output: "json", Owner: "alice"}
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
	if len(rows) != 1 || rows[0]["session"] != "sess-a" {
		t.Fatalf("unexpected status rows: %#v", rows)
	}
	// Resolving the session against the wrong cluster is not just a wrong
	// read: finding neither pod nor workload there clears the active session.
	if hits := currentHits.Load(); hits != 0 {
		t.Fatalf("status resolved the session against the current-context cluster (%d requests)", hits)
	}
	active, err := session.LoadActiveSession()
	if err != nil {
		t.Fatalf("LoadActiveSession: %v", err)
	}
	if active != "sess-a" {
		t.Fatalf("expected the active session to survive status, got %q", active)
	}
}

func TestSessionCompletionHonorsPinnedKubeContextWithNamespaceFlag(t *testing.T) {
	pinned, current, currentHits := startPinnedAndCurrentClusters(t)

	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLIPinnedContextKubeconfig(t, pinned, current))
	cfgPath := writeCLIConfigWithKubeContext(t, "demo", "pinned")

	opts := &Options{ConfigPath: cfgPath, Namespace: "demo", Owner: "alice"}
	got, directive := sessionCompletionFunc(opts)(&cobra.Command{}, nil, "sess-")

	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("unexpected directive %v", directive)
	}
	if len(got) != 1 || got[0] != "sess-a" {
		t.Fatalf("unexpected completions %#v", got)
	}
	if hits := currentHits.Load(); hits != 0 {
		t.Fatalf("--namespace made completion fall back to the current-context cluster (%d requests)", hits)
	}
}

// startPinnedAndCurrentClusters returns the cluster the config pins (holding
// session sess-a) and the one the kubeconfig makes current (empty), plus a
// counter of every request the latter served.
func startPinnedAndCurrentClusters(t *testing.T) (*httptest.Server, *httptest.Server, *atomic.Int64) {
	t.Helper()
	pinned := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(pinned.Close)

	var hits atomic.Int64
	current := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/pods", "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(current.Close)

	return pinned, current, &hits
}

func writeCLIPinnedContextKubeconfig(t *testing.T, pinned, current *httptest.Server) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	content := fmt.Sprintf(`apiVersion: v1
kind: Config
clusters:
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: pinned
- cluster:
    server: %s
    insecure-skip-tls-verify: true
  name: current
contexts:
- context:
    cluster: pinned
    user: dev
  name: pinned
- context:
    cluster: current
    user: dev
  name: current
current-context: current
users:
- name: dev
  user:
    token: test
`, serverURL(t, pinned), serverURL(t, current))
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

func serverURL(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatalf("parse server url: %v", err)
	}
	return (&url.URL{Scheme: u.Scheme, Host: u.Host}).String()
}

func writeCLIConfigWithKubeContext(t *testing.T, namespace, kubeContext string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, ".okdev.yaml")
	content := fmt.Sprintf(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: %s
  kubeContext: %s
  workload:
    type: pod
    manifestPath: pod.yaml
`, namespace, kubeContext)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: okdev-{{ .SessionName }}
spec:
  containers:
    - name: dev
      image: ubuntu:22.04
`
	if err := os.WriteFile(filepath.Join(dir, "pod.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}
