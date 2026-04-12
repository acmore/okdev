package kube

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
)

func TestClientAPIHelpersAgainstHTTPTLSServer(t *testing.T) {
	var patchCount atomic.Int32
	var deleteCount atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods":
			_, _ = io.WriteString(w, `{"kind":"PodList","apiVersion":"v1","items":[{"metadata":{"namespace":"demo","name":"pod-a","creationTimestamp":"2026-03-29T00:00:00Z","labels":{"okdev.io/session":"sess"},"annotations":{"anno":"one"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":1}]}}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods/pod-a":
			_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"pod-a","annotations":{"anno":"one"}},"status":{"phase":"Running","containerStatuses":[{"name":"dev","ready":true,"restartCount":2}]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods/pod-containers":
			_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"pod-containers"},"spec":{"containers":[{"name":"dev"},{"name":"okdev-sidecar"}]}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/persistentvolumeclaims/workspace":
			_, _ = io.WriteString(w, `{"kind":"PersistentVolumeClaim","apiVersion":"v1","metadata":{"name":"workspace","namespace":"demo"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/persistentvolumeclaims/missing":
			http.NotFound(w, r)
		case r.Method == http.MethodGet && r.URL.Path == "/apis/batch/v1/namespaces/demo/jobs/job-a":
			_, _ = io.WriteString(w, `{"kind":"Job","apiVersion":"batch/v1","metadata":{"name":"job-a","namespace":"demo"}}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/events":
			_, _ = io.WriteString(w, `{"kind":"EventList","apiVersion":"v1","items":[{"reason":"Pulled","message":"container image pulled"}]}`)
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/namespaces/demo/pods/pod-a/log":
			_, _ = io.WriteString(w, "line-1\nline-2\n")
		case r.Method == http.MethodPatch && r.URL.Path == "/api/v1/namespaces/demo/pods/pod-a":
			patchCount.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"kind":"Pod","apiVersion":"v1","metadata":{"namespace":"demo","name":"pod-a"}}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/demo/pods/pod-a":
			deleteCount.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		case r.Method == http.MethodDelete && r.URL.Path == "/api/v1/namespaces/demo/persistentvolumeclaims/workspace":
			deleteCount.Add(1)
			w.WriteHeader(http.StatusOK)
			_, _ = io.WriteString(w, `{"kind":"Status","apiVersion":"v1","status":"Success"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()

	client := &Client{Context: "dev"}
	t.Setenv("KUBECONFIG", writeTLSTestKubeconfig(t, server))

	ctx := context.Background()
	pods, err := client.ListPods(ctx, "demo", false, "okdev.io/managed=true")
	if err != nil {
		t.Fatalf("ListPods: %v", err)
	}
	if len(pods) != 1 || pods[0].Name != "pod-a" {
		t.Fatalf("unexpected pods: %+v", pods)
	}

	summary, err := client.GetPodSummary(ctx, "demo", "pod-a")
	if err != nil {
		t.Fatalf("GetPodSummary: %v", err)
	}
	if summary.Name != "pod-a" || summary.Restarts != 2 {
		t.Fatalf("unexpected summary: %+v", summary)
	}

	exists, err := client.PersistentVolumeClaimExists(ctx, "demo", "workspace")
	if err != nil {
		t.Fatalf("PersistentVolumeClaimExists: %v", err)
	}
	if !exists {
		t.Fatal("expected pvc to exist")
	}
	exists, err = client.PersistentVolumeClaimExists(ctx, "demo", "missing")
	if err != nil {
		t.Fatalf("PersistentVolumeClaimExists missing: %v", err)
	}
	if exists {
		t.Fatal("did not expect missing pvc to exist")
	}

	exists, err = client.ResourceExists(ctx, "demo", "v1", "Pod", "pod-a")
	if err != nil {
		t.Fatalf("ResourceExists pod: %v", err)
	}
	if !exists {
		t.Fatal("expected pod to exist")
	}
	exists, err = client.ResourceExists(ctx, "demo", "batch/v1", "Job", "job-a")
	if err != nil {
		t.Fatalf("ResourceExists job: %v", err)
	}
	if !exists {
		t.Fatal("expected job to exist")
	}
	if _, err := client.ResourceExists(ctx, "demo", "::::", "Job", "job-a"); err == nil {
		t.Fatal("expected invalid apiVersion error")
	}

	if err := client.TouchPodActivity(ctx, "demo", "pod-a"); err != nil {
		t.Fatalf("TouchPodActivity: %v", err)
	}
	if err := client.AnnotatePod(ctx, "demo", "pod-a", "key", "value"); err != nil {
		t.Fatalf("AnnotatePod: %v", err)
	}
	if patchCount.Load() != 2 {
		t.Fatalf("expected two patch requests, got %d", patchCount.Load())
	}

	annotation, err := client.GetPodAnnotation(ctx, "demo", "pod-a", "anno")
	if err != nil {
		t.Fatalf("GetPodAnnotation: %v", err)
	}
	if annotation != "one" {
		t.Fatalf("unexpected annotation %q", annotation)
	}

	desc, err := client.DescribePod(ctx, "demo", "pod-a")
	if err != nil {
		t.Fatalf("DescribePod: %v", err)
	}
	for _, want := range []string{"Pod: demo/pod-a", "Phase: Running", "Events:", "Pulled: container image pulled"} {
		if !strings.Contains(desc, want) {
			t.Fatalf("expected describe output to contain %q, got %q", want, desc)
		}
	}

	var logs strings.Builder
	if err := client.StreamPodLogs(ctx, "demo", "pod-a", LogStreamOptions{}, &logs); err != nil {
		t.Fatalf("StreamPodLogs: %v", err)
	}
	if logs.String() != "line-1\nline-2\n" {
		t.Fatalf("unexpected logs %q", logs.String())
	}

	cs, _, err := client.clientset()
	if err != nil {
		t.Fatalf("clientset: %v", err)
	}
	reqURL, err := client.portForwardURL(cs, "demo", "pod-a")
	if err != nil {
		t.Fatalf("portForwardURL: %v", err)
	}
	if reqURL.Path != "/api/v1/namespaces/demo/pods/pod-a/portforward" {
		t.Fatalf("unexpected portforward path %q", reqURL.Path)
	}

	if err := client.DeleteByRef(ctx, "demo", "", "pod", "pod-a", false); err != nil {
		t.Fatalf("DeleteByRef: %v", err)
	}
	if err := client.DeleteByRef(ctx, "demo", "", "persistentvolumeclaim", "workspace", false); err != nil {
		t.Fatalf("DeleteByRef pvc: %v", err)
	}
	if err := client.Delete(ctx, "demo", "pod", "pod-a", false); err != nil {
		t.Fatalf("Delete wrapper: %v", err)
	}
	if deleteCount.Load() != 3 {
		t.Fatalf("expected three delete requests, got %d", deleteCount.Load())
	}

	containers, err := client.PodContainerNames(ctx, "demo", "pod-containers")
	if err != nil {
		t.Fatalf("PodContainerNames: %v", err)
	}
	if len(containers) != 2 || containers[0] != "dev" || containers[1] != "okdev-sidecar" {
		t.Fatalf("unexpected containers %v", containers)
	}
}

func TestWaitForDeletionAndIsPodReady(t *testing.T) {
	readyPod := &corev1.Pod{
		Status: corev1.PodStatus{
			Conditions: []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}},
		},
	}
	if !isPodReady(readyPod) {
		t.Fatal("expected pod to be ready")
	}
	if isPodReady(&corev1.Pod{}) {
		t.Fatal("did not expect empty pod to be ready")
	}
}

func TestDeletionWaitContextUsesDefaultTimeout(t *testing.T) {
	previous := deletionWaitTimeout
	deletionWaitTimeout = 20 * time.Millisecond
	t.Cleanup(func() {
		deletionWaitTimeout = previous
	})

	ctx, cancel := deletionWaitContext(context.Background())
	defer cancel()

	if _, ok := ctx.Deadline(); !ok {
		t.Fatal("expected default deletion wait deadline")
	}
	<-ctx.Done()
	if !errors.Is(ctx.Err(), context.DeadlineExceeded) {
		t.Fatalf("expected deadline exceeded, got %v", ctx.Err())
	}
}

func writeTLSTestKubeconfig(t *testing.T, srv *httptest.Server) string {
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
