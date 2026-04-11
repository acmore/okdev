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
	if vars.DevImage != "ubuntu:22.04" {
		t.Fatalf("expected default dev image, got %q", vars.DevImage)
	}
	if vars.DevCPURequest != "500m" || vars.DevMemoryRequest != "512Mi" || vars.DevCPULimit != "500m" || vars.DevMemoryLimit != "512Mi" {
		t.Fatalf("unexpected default dev resources: %#v", vars)
	}
	if vars.SidecarCPU != "250m" || vars.SidecarMemory != "512Mi" {
		t.Fatalf("unexpected default sidecar resources: %#v", vars)
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
	if !strings.Contains(out, "{{ .Repo }}-{{ .User }}") {
		t.Fatal("expected session name template to be preserved literally")
	}
	if strings.Contains(out, "\n    exclude:\n") {
		t.Fatalf("expected starter template to keep local ignore rules out of .okdev.yaml, got:\n%s", out)
	}
	if !strings.Contains(out, "podTemplate:") {
		t.Fatalf("expected default pod template, got:\n%s", out)
	}
	if !strings.Contains(out, "image: ubuntu:22.04") {
		t.Fatalf("expected default dev image, got:\n%s", out)
	}
	if !strings.Contains(out, "cpu: \"500m\"") || !strings.Contains(out, "memory: 512Mi") {
		t.Fatalf("expected default dev resource requests, got:\n%s", out)
	}
	if strings.Count(out, "cpu: \"500m\"") != 2 || strings.Count(out, "memory: 512Mi") != 4 {
		t.Fatalf("expected default dev resource limits, got:\n%s", out)
	}
	if strings.Count(out, "cpu: \"250m\"") != 2 {
		t.Fatalf("expected equal default sidecar CPU request/limit, got:\n%s", out)
	}
}

func TestBuiltinTemplateLocalIgnores(t *testing.T) {
	if got := BuiltinTemplateLocalIgnores(""); len(got) == 0 {
		t.Fatal("expected empty template ref to use basic local ignore patterns")
	}
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

func TestResolveTemplateProjectLocal(t *testing.T) {
	projDir := t.TempDir()
	tmplDir := filepath.Join(projDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmplDir, "team.yaml.tmpl"), "name: {{ .Name }}\nproject-local: true")

	content, err := ResolveTemplateFromDir(context.Background(), "team", projDir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(content, "project-local: true") {
		t.Fatalf("expected project-local template, got %q", content)
	}
}

func TestResolveTemplateProjectLocalShadowsUser(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	userDir := filepath.Join(home, ".okdev", "templates")
	if err := os.MkdirAll(userDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(userDir, "team.yaml.tmpl"), "source: user")

	projDir := t.TempDir()
	tmplDir := filepath.Join(projDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmplDir, "team.yaml.tmpl"), "source: project")

	content, err := ResolveTemplateFromDir(context.Background(), "team", projDir)
	if err != nil {
		t.Fatal(err)
	}
	if content != "source: project" {
		t.Fatalf("expected project-local to shadow user, got %q", content)
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

func TestUserTemplateNamesEmptyDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	names, err := UserTemplateNames()
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("expected no user templates, got %v", names)
	}
}

func TestUserTemplateNamesFiltersNonTemplates(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	registryDir := filepath.Join(home, ".okdev", "templates")
	if err := os.MkdirAll(registryDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(registryDir, "valid.yaml.tmpl"), "ok")
	writeFile(t, filepath.Join(registryDir, "readme.md"), "skip")
	writeFile(t, filepath.Join(registryDir, "backup.yaml.bak"), "skip")

	names, err := UserTemplateNames()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"valid"}) {
		t.Fatalf("expected [valid], got %v", names)
	}
}

func TestProjectTemplateNames(t *testing.T) {
	projDir := t.TempDir()
	tmplDir := filepath.Join(projDir, ".okdev", "templates")
	if err := os.MkdirAll(tmplDir, 0o755); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(tmplDir, "team-a.yaml.tmpl"), "ok")
	writeFile(t, filepath.Join(tmplDir, "team-b.yaml.tmpl"), "ok")
	writeFile(t, filepath.Join(tmplDir, "readme.md"), "skip")

	names, err := ProjectTemplateNames(projDir)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(names, []string{"team-a", "team-b"}) {
		t.Fatalf("expected [team-a team-b], got %v", names)
	}
}

func TestProjectTemplateNamesNoDir(t *testing.T) {
	names, err := ProjectTemplateNames(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if len(names) != 0 {
		t.Fatalf("expected empty, got %v", names)
	}
}

func TestResolveUserTemplateRejectsTraversal(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	_, err := resolveUserTemplate("../../../etc/passwd")
	if err == nil {
		t.Fatal("expected path traversal error")
	}
	if !strings.Contains(err.Error(), "resolves outside registry") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestRenderTemplateWithSprigFunctions(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "sprig.yaml.tmpl")
	writeFile(t, tmplPath, "name: {{ .Name | upper }}\nimage: {{ .DevImage | default \"fallback\" }}")

	vars := NewTemplateVars()
	vars.Name = "demo"
	vars.DevImage = ""
	out, err := RenderTemplate(tmplPath, vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "name: DEMO") {
		t.Fatalf("expected upper-cased name, got %q", out)
	}
	if !strings.Contains(out, "image: fallback") {
		t.Fatalf("expected default fallback, got %q", out)
	}
}

func TestRenderTemplateWithCustomVars(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "custom.yaml.tmpl")
	writeFile(t, tmplPath, "---\nvariables:\n  - name: numWorkers\n    type: int\n    default: 2\n---\nname: {{ .Name }}\nworkers: {{ .Vars.numWorkers }}")

	vars := NewTemplateVars()
	vars.Name = "demo"
	out, err := RenderTemplateWithVars(context.Background(), tmplPath, vars, map[string]any{"numWorkers": 4}, "")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "workers: 4") || !strings.Contains(out, "name: demo") {
		t.Fatalf("unexpected rendered output: %q", out)
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
