package kube

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"k8s.io/client-go/kubernetes/scheme"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	authorizationv1 "k8s.io/api/authorization/v1"
)

func execAccessConfig(t *testing.T, server, tokenFile string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config")
	data := fmt.Sprintf(`{"apiVersion":"v1","kind":"Config","current-context":"test","contexts":[{"name":"test","context":{"cluster":"test","user":"test"}}],"clusters":[{"name":"test","cluster":{"server":%q}}],"users":[{"name":"test","user":{"tokenFile":%q}}]}`, server, tokenFile)
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("KUBECONFIG", path)
	return path
}

func TestSSHExecAuthorizationChecksEveryCallAndFailsClosed(t *testing.T) {
	allowed, evaluationError := true, ""
	var resources []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var review authorizationv1.SelfSubjectAccessReview
		if r.Method == "GET" {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprint(w, `{"apiVersion":"v1","kind":"APIGroupList","groups":[]}`)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if _, _, err := scheme.Codecs.UniversalDeserializer().Decode(body, nil, &review); err != nil {
			t.Error(err)
			w.WriteHeader(400)
			return
		}
		a := review.Spec.ResourceAttributes
		if a.Namespace != "ns" || a.Name != "pod" || (a.Verb != "create" && a.Verb != "get") || a.Resource != "pods" {
			t.Errorf("unexpected attributes: %+v", a)
		}
		resources = append(resources, a.Subresource+"/"+a.Verb)
		review.APIVersion = "authorization.k8s.io/v1"
		review.Kind = "SelfSubjectAccessReview"
		review.Status = authorizationv1.SubjectAccessReviewStatus{Allowed: allowed, EvaluationError: evaluationError}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(review)
	}))
	defer server.Close()
	execAccessConfig(t, server.URL, "")
	client := &Client{}
	for range 2 {
		if err := client.AuthorizeSSHExec(context.Background(), "ns", "pod"); err != nil {
			t.Fatal(err)
		}
	}
	if fmt.Sprint(resources) != "[exec/get exec/create portforward/get portforward/create exec/get exec/create portforward/get portforward/create]" {
		t.Fatal(resources)
	}
	allowed = false
	if err := client.AuthorizeSSHExec(context.Background(), "ns", "pod"); err == nil {
		t.Fatal("authorization revocation ignored")
	}
	allowed = true
	evaluationError = "cannot resolve policy"
	if err := client.AuthorizeSSHExec(context.Background(), "ns", "pod"); err == nil {
		t.Fatal("incomplete authorization accepted")
	}
	server.Close()
	if err := client.AuthorizeSSHExec(context.Background(), "ns", "pod"); err == nil {
		t.Fatal("unavailable authorization accepted")
	}
}

func TestSSHConnectionIdentityIncludesTokenFileContents(t *testing.T) {
	token := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(token, []byte("first"), 0600); err != nil {
		t.Fatal(err)
	}
	execAccessConfig(t, "https://example.invalid", token)
	client := &Client{}
	first, err := client.ConnectionIdentity()
	if err != nil {
		t.Fatal(err)
	}
	same, err := client.ConnectionIdentity()
	if err != nil || same != first {
		t.Fatalf("unstable identity: %v", err)
	}
	if err := os.WriteFile(token, []byte("second"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := client.ConnectionIdentity()
	if err != nil || second == first {
		t.Fatalf("token rotation did not invalidate identity: %v", err)
	}
}
