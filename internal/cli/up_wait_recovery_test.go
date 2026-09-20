package cli

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

type waitErrorRuntime struct {
	fakeRefRuntime
	err error
}

func (r *waitErrorRuntime) WaitReady(context.Context, workload.WaitClient, string, time.Duration, func(kube.PodReadinessProgress)) error {
	return r.err
}

func TestUpWaitTimeoutExplainsRetainedWorkloadAndExactRecovery(t *testing.T) {
	for _, tc := range []struct {
		name         string
		err          error
		wantRetained bool
	}{
		{"deadline", fmt.Errorf("readiness: %w", context.DeadlineExceeded), true},
		{"cancelled", context.Canceled, true},
		{"forbidden", apierrors.NewForbidden(schema.GroupResource{Resource: "pods"}, "target", errors.New("denied")), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if r.URL.Path == "/api/v1/namespaces/team/pods" {
					_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
					return
				}
				http.NotFound(w, r)
			}))
			defer server.Close()
			t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
			opts := &Options{ConfigPath: "/tmp/project with spaces/env's.yaml", Session: "queued-a", Context: "dev", Namespace: "team", Owner: "alice", Workload: "batch"}
			cmd := newUpCmd(opts)
			if err := cmd.Flags().Set("workload", "batch"); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			cmd.SetOut(&out)
			cmd.SetErr(&stderr)
			runtime := &waitErrorRuntime{fakeRefRuntime: fakeRefRuntime{kind: "job", name: "trainer"}, err: tc.err}
			state := &upState{cmd: cmd, opts: opts, flags: upOptions{waitTimeout: 5 * time.Minute, waitHooks: true, noTmux: true, reconcile: true, resetWorkspace: true}, ui: newUpUI(&out, &stderr), ctx: context.Background(), runtime: runtime, workloadName: "trainer", command: &commandContext{opts: opts, cfg: &config.DevEnvironment{}, cfgPath: opts.ConfigPath, sessionName: opts.Session, namespace: "team", kube: &kube.Client{Context: "dev"}}}
			err := upWait(state)
			if err == nil {
				t.Fatal("expected readiness error")
			}
			text := stderr.String()
			if tc.wantRetained {
				for _, want := range []string{"workload remains submitted", "no background watcher", "resume waiting and setup", "--config '/tmp/project with spaces/env'\\''s.yaml'", "--context 'dev'", "--namespace 'team'", "--session 'queued-a'", "--owner 'alice'", "up --wait-timeout '5m0s'", "--workload 'batch'", "--wait-hooks", "--no-tmux", "down --yes"} {
					if !strings.Contains(text, want) {
						t.Errorf("missing %q in %s", want, text)
					}
				}
				for _, unsafe := range []string{"--reconcile", "--reset-workspace"} {
					if strings.Contains(text, unsafe) {
						t.Errorf("resume replays mutating option %q", unsafe)
					}
				}
			} else if strings.Contains(text, "resume waiting and setup") {
				t.Fatalf("terminal auth error presented as wait expiration: %s", text)
			}
			if runtime.deleteCall != 0 {
				t.Fatal("wait error deleted workload")
			}
		})
	}
}

func TestUpValidatePinsCurrentContextForWaitRecovery(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[]}`)
	}))
	defer server.Close()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("KUBECONFIG", writeCLITLSTestKubeconfig(t, server))
	opts := &Options{ConfigPath: writeCLIConfig(t, "demo"), Session: "queued"}
	cmd := newUpCmd(opts)
	cmd.SetContext(context.Background())
	cmd.SetOut(io.Discard)
	cmd.SetErr(io.Discard)
	state, err := upValidate(cmd, opts, upOptions{waitTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer state.cancel()
	if state.opts.Context != "dev" || state.command.kube.Context != "dev" {
		t.Fatalf("resolved context not pinned: opts=%q client=%q", state.opts.Context, state.command.kube.Context)
	}
	if opts.Context != "" {
		t.Fatal("caller options mutated")
	}
}
