package config

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestDefaultTemplateVars(t *testing.T) {
	vars := NewTemplateVars()
	if vars.Namespace != "default" {
		t.Fatalf("expected default namespace, got %q", vars.Namespace)
	}
	if vars.SSHUser != "root" {
		t.Fatalf("expected root ssh user, got %q", vars.SSHUser)
	}
	if vars.SyncLocal != "." {
		t.Fatalf("expected . sync local, got %q", vars.SyncLocal)
	}
	if vars.SyncRemote != "/workspace" {
		t.Fatalf("expected /workspace sync remote, got %q", vars.SyncRemote)
	}
	if vars.WorkloadType != "pod" {
		t.Fatalf("expected pod workload type, got %q", vars.WorkloadType)
	}
}

type fakeTemplateHTTPDoer struct {
	resp *http.Response
	err  error
}

func (f fakeTemplateHTTPDoer) Do(*http.Request) (*http.Response, error) {
	return f.resp, f.err
}

func TestRenderBuiltinBasic(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "test-project"
	out, err := RenderTemplate("basic", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: test-project") {
		t.Fatal("expected rendered name in output")
	}
	if !strings.Contains(out, "namespace: default") {
		t.Fatal("expected namespace in output")
	}
	// Verify runtime template syntax is preserved literally
	if !strings.Contains(out, "{{ .Repo }}-{{ .Branch }}-{{ .User }}") {
		t.Fatal("expected session name template to be preserved literally")
	}
	if strings.Contains(out, "\n    exclude:\n") {
		t.Fatalf("expected starter template to keep local ignore rules out of .okdev.yaml, got:\n%s", out)
	}
}

func TestBuiltinTemplateLocalIgnores(t *testing.T) {
	patterns := BuiltinTemplateLocalIgnores("basic")
	if len(patterns) == 0 {
		t.Fatal("expected built-in local ignore patterns")
	}
	if patterns[0] != ".git/" {
		t.Fatalf("unexpected first pattern %q", patterns[0])
	}
	if got := BuiltinTemplateLocalIgnores("./custom.yaml"); got != nil {
		t.Fatalf("expected nil ignores for custom template, got %+v", got)
	}
}

func TestSTIgnorePreset(t *testing.T) {
	patterns := STIgnorePreset("go")
	if len(patterns) == 0 {
		t.Fatal("expected go preset patterns")
	}
	if patterns[1] != "bin/" {
		t.Fatalf("unexpected go preset pattern %q", patterns[1])
	}
	if got := STIgnorePreset("missing"); got != nil {
		t.Fatalf("expected nil for unknown preset, got %+v", got)
	}
}

func TestResolveTemplateBuiltinName(t *testing.T) {
	content, err := ResolveTemplate("basic")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "{{ .Name }}") {
		t.Fatal("expected template variable in raw content")
	}
}

func TestResolveTemplateUserRegistry(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registryDir := filepath.Join(home, ".okdev", "templates")
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatalf("mkdir registry: %v", err)
	}
	writeFile(t, filepath.Join(registryDir, "team.yaml.tmpl"), "name: {{ .Name }}")

	content, err := ResolveTemplate("team")
	if err != nil {
		t.Fatal(err)
	}
	if content != "name: {{ .Name }}" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestResolveTemplateFilePath(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := tmp + "/custom.yaml.tmpl"
	writeFile(t, tmplPath, "name: {{ .Name }}")
	content, err := ResolveTemplate(tmplPath)
	if err != nil {
		t.Fatal(err)
	}
	if content != "name: {{ .Name }}" {
		t.Fatalf("unexpected content: %q", content)
	}
}

func TestResolveTemplateContextCancelsURLFetch(t *testing.T) {
	blocked := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(blocked)
		<-r.Context().Done()
	}))
	defer srv.Close()

	ctx, cancel := context.WithCancel(context.Background())
	errCh := make(chan error, 1)
	go func() {
		_, err := ResolveTemplateContext(ctx, srv.URL)
		errCh <- err
	}()

	select {
	case <-blocked:
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request to start")
	}
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("expected cancellation error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("expected context cancellation, got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for canceled fetch to return")
	}
}

func TestResolveTemplateContextUsesInjectableHTTPClient(t *testing.T) {
	old := templateHTTPClient
	t.Cleanup(func() { templateHTTPClient = old })
	templateHTTPClient = fakeTemplateHTTPDoer{
		resp: &http.Response{
			StatusCode: http.StatusOK,
			Body:       io.NopCloser(strings.NewReader("name: {{ .Name }}")),
		},
	}
	got, err := ResolveTemplateContext(context.Background(), "https://example.com/template.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if got != "name: {{ .Name }}" {
		t.Fatalf("unexpected template body %q", got)
	}
}

func TestRenderTemplateContextFetchesURLTemplate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("name: {{ .Name }}\nnamespace: {{ .Namespace }}\n"))
	}))
	defer srv.Close()

	vars := NewTemplateVars()
	vars.Name = "demo"
	out, err := RenderTemplateContext(context.Background(), srv.URL, vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: demo") {
		t.Fatalf("expected rendered name, got %q", out)
	}
	if !strings.Contains(out, "namespace: default") {
		t.Fatalf("expected rendered namespace, got %q", out)
	}
}

func TestBuiltinTemplateNames(t *testing.T) {
	if got := BuiltinTemplateNames(); !slices.Equal(got, []string{"basic"}) {
		t.Fatalf("unexpected builtins: %+v", got)
	}
}

func TestRenderEmbeddedTemplate(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "demo"
	out, err := RenderEmbeddedTemplate("templates/manifests/job.yaml.tmpl", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kind: Job") {
		t.Fatalf("expected job manifest, got %q", out)
	}
	if !strings.Contains(out, "name: demo") {
		t.Fatalf("expected rendered name, got %q", out)
	}
}
