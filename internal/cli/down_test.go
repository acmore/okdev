package cli

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
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
