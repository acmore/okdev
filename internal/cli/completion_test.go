package cli

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/spf13/cobra"
)

func TestSessionCompletionFuncReturnsOwnedSessions(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[
{"metadata":{"namespace":"demo","name":"okdev-sess-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess-a","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}},
{"metadata":{"namespace":"demo","name":"okdev-sess-b","creationTimestamp":"2026-03-29T00:01:00Z","labels":{"okdev.io/session":"sess-b","okdev.io/owner":"alice","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}},
{"metadata":{"namespace":"demo","name":"okdev-other","creationTimestamp":"2026-03-29T00:02:00Z","labels":{"okdev.io/session":"other","okdev.io/owner":"bob","okdev.io/workload-type":"pod"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true}]}}
]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	cfgPath := writeCLIConfig(t, "demo")
	opts := &Options{ConfigPath: cfgPath, Context: "dev", Owner: "alice"}

	got, directive := sessionCompletionFunc(opts)(&cobra.Command{}, nil, "sess-")

	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("unexpected directive %v", directive)
	}
	if len(got) != 2 || got[0] != "sess-a" || got[1] != "sess-b" {
		t.Fatalf("unexpected completions %#v", got)
	}
}

func TestSessionCompletionFuncStopsAfterPositionalArg(t *testing.T) {
	got, directive := sessionCompletionFunc(&Options{})(&cobra.Command{}, []string{"sess-a"}, "")
	if directive != cobra.ShellCompDirectiveNoFileComp {
		t.Fatalf("unexpected directive %v", directive)
	}
	if len(got) != 0 {
		t.Fatalf("expected no further completions, got %#v", got)
	}
}
