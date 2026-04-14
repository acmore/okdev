package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestNewDownCmdDeprecatesDeletePVC(t *testing.T) {
	cmd := newDownCmd(&Options{})
	flag := cmd.Flags().Lookup("delete-pvc")
	if flag == nil {
		t.Fatal("expected delete-pvc flag")
	}
	if flag.Deprecated == "" {
		t.Fatal("expected delete-pvc to be marked deprecated")
	}
}

func TestNewDownCmdHasYesFlag(t *testing.T) {
	cmd := newDownCmd(&Options{})
	flag := cmd.Flags().Lookup("yes")
	if flag == nil {
		t.Fatal("expected yes flag")
	}
	if flag.Shorthand != "y" {
		t.Fatalf("expected shorthand -y, got %q", flag.Shorthand)
	}
}

func TestNewDownCmdHasWaitFlags(t *testing.T) {
	cmd := newDownCmd(&Options{})
	waitFlag := cmd.Flags().Lookup("wait")
	if waitFlag == nil {
		t.Fatal("expected wait flag")
	}
	waitTimeoutFlag := cmd.Flags().Lookup("wait-timeout")
	if waitTimeoutFlag == nil {
		t.Fatal("expected wait-timeout flag")
	}
}

func TestPromptConfirmDownAccepts(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n", "YES\n", " y \n"} {
		in := strings.NewReader(input)
		var out bytes.Buffer
		ok, err := promptConfirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", input, err)
		}
		if !ok {
			t.Fatalf("input %q: expected confirmation", input)
		}
		if !strings.Contains(out.String(), "my-session") {
			t.Fatalf("expected prompt to contain session name, got %q", out.String())
		}
	}
}

func TestPromptConfirmDownRejects(t *testing.T) {
	for _, input := range []string{"n\n", "N\n", "no\n", "\n", "maybe\n"} {
		in := strings.NewReader(input)
		var out bytes.Buffer
		ok, err := promptConfirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", input, err)
		}
		if ok {
			t.Fatalf("input %q: expected rejection", input)
		}
	}
}

func TestConfirmDownRejectsNonTTY(t *testing.T) {
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	_, err := confirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
	if err == nil {
		t.Fatal("expected error for non-TTY input")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected error to mention --yes, got %q", err.Error())
	}
}

func TestNewDownCmdDryRunOutputsJSON(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			_, _ = io.WriteString(w, `{"metadata":{"namespace":"demo","name":"okdev-sess-a","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("down execute: %v", err)
	}

	var payload downOutput
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if !payload.DryRun || payload.Deleted || payload.Status != "planned" {
		t.Fatalf("unexpected dry-run payload: %#v", payload)
	}
	if payload.Session != "sess-a" || payload.Namespace != "demo" || payload.Kind != "pod" || payload.Workload != "okdev-sess-a" {
		t.Fatalf("unexpected payload identity: %#v", payload)
	}
}

func TestNewDownCmdSkipsMissingWorkloadWithoutConfirmation(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"other","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"other","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetIn(strings.NewReader("n\n"))

	if err := cmd.Execute(); err != nil {
		t.Fatalf("down execute: %v", err)
	}

	var payload downOutput
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if payload.Deleted {
		t.Fatalf("expected deleted=false, got %#v", payload)
	}
	if payload.Status != "already stopped" {
		t.Fatalf("expected status already stopped, got %#v", payload)
	}
	for _, key := range []string{"localClients", "sync", "syncthing", "sshForward", "sshConfig", "target", "syncState"} {
		if _, ok := payload.Cleanup[key]; !ok {
			t.Fatalf("expected cleanup key %q in payload: %#v", key, payload)
		}
	}
}

func TestNewDownCmdDryRunReportsMissingWorkload(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"other","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"other","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--dry-run"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("down execute: %v", err)
	}

	var payload downOutput
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if payload.Status != "already stopped" {
		t.Fatalf("expected status already stopped, got %#v", payload)
	}
	if len(payload.Notes) == 0 || payload.Notes[0] != "session workload already absent" {
		t.Fatalf("expected absent-workload note, got %#v", payload)
	}
}

func TestNewDownCmdDryRunOutputsJSONWithWait(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
		case "/api/v1/namespaces/demo/pods/okdev-sess-a":
			_, _ = io.WriteString(w, `{"metadata":{"namespace":"demo","name":"okdev-sess-a","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--dry-run", "--wait", "--wait-timeout", "2m"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("down execute: %v", err)
	}

	var payload downOutput
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if !payload.DryRun {
		t.Fatalf("expected dry-run payload: %#v", payload)
	}
	if payload.Wait == nil || !payload.Wait.Enabled {
		t.Fatalf("expected wait enabled in payload: %#v", payload)
	}
	if payload.Wait.Timeout != "2m0s" {
		t.Fatalf("expected wait timeout 2m0s, got %#v", payload.Wait)
	}
}

func TestNewDownCmdWaitsForPodDeletion(t *testing.T) {
	var getCount int
	var listCount int
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods":
			listCount++
			if listCount == 1 {
				_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/name":"demo","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}]}`)
				return
			}
			if listCount == 2 {
				_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","deletionTimestamp":"2026-03-29T00:01:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/name":"demo","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":false}]}}]}`)
				return
			}
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods/okdev-sess-a":
			getCount++
			if getCount <= 2 {
				_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"okdev-sess-a","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/name":"demo","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}`)
				return
			}
			http.NotFound(w, r)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/demo/pods/okdev-sess-a":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes", "--wait", "--wait-timeout", "2s"})

	if err := cmd.Execute(); err != nil {
		t.Fatalf("down execute: %v", err)
	}

	var payload downOutput
	if err := json.Unmarshal(out.Bytes(), &payload); err != nil {
		t.Fatalf("json unmarshal: %v\n%s", err, out.String())
	}
	if payload.Wait == nil || !payload.Wait.Enabled || payload.Wait.Status != "completed" {
		t.Fatalf("expected completed wait payload, got %#v", payload)
	}
	if !payload.Wait.WorkloadDeleted || !payload.Wait.PodsDeleted {
		t.Fatalf("expected workload and pod deletion observed, got %#v", payload.Wait)
	}
}

func TestNewDownCmdWaitTimeoutReturnsError(t *testing.T) {
	previousInterval := downWaitPollInterval
	downWaitPollInterval = 5 * time.Millisecond
	t.Cleanup(func() {
		downWaitPollInterval = previousInterval
	})

	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","deletionTimestamp":"2026-03-29T00:01:00Z","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/name":"demo","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":false}]}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods/okdev-sess-a":
			_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"okdev-sess-a","labels":{"okdev.io/managed":"true","okdev.io/session":"sess-a","okdev.io/name":"demo","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":false}]}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/demo/pods/okdev-sess-a":
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Session: "sess-a", Owner: "alice", Output: "json"}
	cmd := newDownCmd(opts)
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(io.Discard)
	cmd.SetArgs([]string{"--yes", "--wait", "--wait-timeout", "20ms"})

	err := cmd.Execute()
	if err == nil {
		t.Fatal("expected wait timeout error")
	}
	if !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("expected timeout error, got %v", err)
	}
}
