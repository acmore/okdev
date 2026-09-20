package workload

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestReadinessRelistsAfterRealWatchInterruption(t *testing.T) {
	for _, failure := range []string{"closed stream", "expired version"} {
		t.Run(failure, func(t *testing.T) {
			var gets, watches atomic.Int32
			var ready atomic.Bool
			pod := func(version string) string {
				phase, condition := "Pending", "False"
				if ready.Load() {
					phase, condition = "Running", "True"
				}
				return fmt.Sprintf(`{"kind":"Pod","apiVersion":"v1","metadata":{"name":"target","namespace":"default","resourceVersion":%q},"spec":{"containers":[{"name":"dev"}]},"status":{"phase":%q,"conditions":[{"type":"Ready","status":%q}],"containerStatuses":[{"name":"dev","ready":%v}]}}`, version, phase, condition, ready.Load())
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				switch {
				case r.URL.Query().Get("watch") == "true":
					attempt := watches.Add(1)
					if attempt == 1 {
						w.WriteHeader(http.StatusOK)
						if failure == "expired version" {
							_, _ = io.WriteString(w, `{"type":"ERROR","object":{"apiVersion":"v1","kind":"Status","status":"Failure","reason":"Expired","message":"old version","code":410}}`+"\n")
						}
						w.(http.Flusher).Flush()
						return
					}
					if r.URL.Query().Get("resourceVersion") != "2" {
						t.Errorf("watch reused stale resourceVersion: %s", r.URL.RawQuery)
					}
					ready.Store(true)
					_, _ = fmt.Fprintf(w, "{\"type\":\"MODIFIED\",\"object\":%s}\n", pod("3"))
				case strings.HasSuffix(r.URL.Path, "/pods/target"):
					version := gets.Add(1)
					_, _ = io.WriteString(w, pod(fmt.Sprint(version)))
				default:
					_, _ = fmt.Fprintf(w, `{"kind":"PodList","apiVersion":"v1","items":[%s]}`, pod("1"))
				}
			}))
			defer server.Close()
			path := filepath.Join(t.TempDir(), "kubeconfig")
			config := fmt.Sprintf("apiVersion: v1\nkind: Config\nclusters:\n- name: test\n  cluster:\n    server: %s\n    insecure-skip-tls-verify: true\ncontexts:\n- name: test\n  context:\n    cluster: test\n    user: test\ncurrent-context: test\nusers:\n- name: test\n  user: {}\n", server.URL)
			if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
				t.Fatal(err)
			}
			t.Setenv("KUBECONFIG", path)
			client := &kube.Client{Context: "test"}
			selector := func(ctx context.Context, k podLister, ns string) (TargetRef, []kube.PodSummary, error) {
				pods, err := k.ListPods(ctx, ns, false, "")
				return TargetRef{PodName: "target"}, pods, err
			}
			if err := waitForCandidatePodReady(context.Background(), client, "default", selector, 3*time.Second, nil, false, "timed out"); err != nil {
				t.Fatal(err)
			}
			if gets.Load() != 2 || watches.Load() != 2 || !ready.Load() {
				t.Fatalf("recovery reads=%d watches=%d ready=%v", gets.Load(), watches.Load(), ready.Load())
			}
		})
	}
}
