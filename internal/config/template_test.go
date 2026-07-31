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

	sigsyaml "sigs.k8s.io/yaml"
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
	// The dev container lives in its own manifest now; the config only points
	// at it. Everything the config still owns must survive.
	if strings.Contains(out, "podTemplate:") {
		t.Fatalf("the config template must not define a workload inline, got:\n%s", out)
	}
	if !strings.Contains(out, "type: pod") {
		t.Fatalf("expected the workload type, got:\n%s", out)
	}
	if strings.Count(out, "cpu: \"250m\"") != 2 || strings.Count(out, "memory: 512Mi") != 2 {
		t.Fatalf("expected equal default sidecar request/limit, got:\n%s", out)
	}
}

// The dev container's shape moved to the pod manifest template, so that is
// where --dev-image and the resource defaults have to land.
func TestRenderBuiltinPodManifest(t *testing.T) {
	vars := NewTemplateVars()
	out, err := RenderEmbeddedTemplate("templates/manifests/pod.yaml.tmpl", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "image: ubuntu:22.04") {
		t.Fatalf("expected default dev image, got:\n%s", out)
	}
	if !strings.Contains(out, "mountPath: /workspace") {
		t.Fatalf("expected the workspace mount, got:\n%s", out)
	}
	if !strings.Contains(out, "{{ .WorkloadName }}") {
		t.Fatalf("expected the runtime name placeholder to survive, got:\n%s", out)
	}
	if strings.Count(out, "cpu: \"500m\"") != 2 || strings.Count(out, "memory: 512Mi") != 2 {
		t.Fatalf("expected equal default dev request/limit for Guaranteed QoS, got:\n%s", out)
	}
}

func TestBuiltinTemplateDeclaresItsSTIgnorePreset(t *testing.T) {
	// The preset is frontmatter, not a name lookup, so a user template shadowing
	// this name inherits nothing.
	raw, err := ResolveTemplate("basic")
	if err != nil {
		t.Fatal(err)
	}
	meta, _, err := ParseFrontmatter(raw)
	if err != nil {
		t.Fatal(err)
	}
	if meta.StignorePreset != "default" {
		t.Fatalf("basic must declare stignorePreset, got %q", meta.StignorePreset)
	}
	if len(STIgnorePreset(meta.StignorePreset)) == 0 {
		t.Fatalf("declared preset %q resolves to nothing", meta.StignorePreset)
	}
}

func TestSTIgnorePreset(t *testing.T) {
	patterns := STIgnorePreset("go")
	if len(patterns) == 0 {
		t.Fatal("expected go preset patterns")
	}
	if patterns[0] != "bin/" {
		t.Fatalf("unexpected go preset pattern %q", patterns[0])
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
	want := []string{"basic", "deployment", "generic", "job", "pod", "pytorchjob"}
	if got := BuiltinTemplateNames(); !slices.Equal(got, want) {
		t.Fatalf("builtins = %+v, want %+v", got, want)
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
	if !strings.Contains(out, "name: {{ .WorkloadName }}") {
		t.Fatalf("expected runtime workload name placeholder, got %q", out)
	}
}

func TestBasicTemplateRendersKubeContext(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "demo"
	vars.KubeContext = "my-cluster"
	out, err := RenderTemplate("basic", vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "kubeContext: my-cluster") {
		t.Fatalf("expected kubeContext in rendered output, got %q", out)
	}
}

func TestBasicTemplateOmitsKubeContextWhenEmpty(t *testing.T) {
	vars := NewTemplateVars()
	vars.Name = "demo"
	out, err := RenderTemplate("basic", vars)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out, "kubeContext") {
		t.Fatalf("expected no kubeContext when empty, got %q", out)
	}
}

func TestNewTemplateVarsPopulatesUserAndRepo(t *testing.T) {
	vars := NewTemplateVars()
	if vars.User == "" {
		t.Fatal("expected User to be populated")
	}
	if vars.Repo == "" {
		t.Fatal("expected Repo to be populated")
	}
}

func TestTemplateCanUseUserAndRepoVars(t *testing.T) {
	tmp := t.TempDir()
	tmplPath := filepath.Join(tmp, "custom.yaml.tmpl")
	writeFile(t, tmplPath, "user: {{ .User }}\nrepo: {{ .Repo }}")

	vars := NewTemplateVars()
	vars.User = "alice"
	vars.Repo = "my-project"
	out, err := RenderTemplate(tmplPath, vars)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "user: alice") {
		t.Fatalf("expected user in output, got %q", out)
	}
	if !strings.Contains(out, "repo: my-project") {
		t.Fatalf("expected repo in output, got %q", out)
	}
}

// Every built-in must render a config that validates and declare a manifest for
// the workload it describes. Config-valid *and* manifest-present is the pairing
// whose absence let a broken scaffold ship before.
func TestEveryBuiltinTemplateDeclaresItsWorkloadManifest(t *testing.T) {
	for _, name := range BuiltinTemplateNames() {
		t.Run(name, func(t *testing.T) {
			raw, err := ResolveTemplate(name)
			if err != nil {
				t.Fatal(err)
			}
			meta, body, err := ParseFrontmatter(raw)
			if err != nil {
				t.Fatal(err)
			}
			vars := NewTemplateVars()
			vars.Name = "demo"
			// "generic" is the bring-your-own-manifest shape; the others carry
			// their own, so only it needs these supplied.
			vars.ManifestPath = "mine.yaml"
			vars.InjectPaths = []string{"spec.template"}
			out, err := RenderTemplateContent("okdev", body, vars, nil)
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			var cfg DevEnvironment
			if err := sigsyaml.Unmarshal([]byte(out), &cfg); err != nil {
				t.Fatalf("parse rendered config: %v\n%s", err, out)
			}
			cfg.SetDefaults()
			if err := cfg.Validate(); err != nil {
				t.Fatalf("rendered config invalid: %v\n%s", err, out)
			}
			if name == "generic" {
				if len(meta.Files) != 0 {
					t.Fatalf("generic brings its own manifest and must declare no files, got %+v", meta.Files)
				}
				return
			}
			// The declared companion file must be the workload's manifest.
			base := filepath.Base(cfg.Spec.Workload.ManifestPath)
			found := false
			for _, f := range meta.Files {
				if filepath.Base(f.Path) == base {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s declares manifestPath %q but no matching file: %+v",
					name, cfg.Spec.Workload.ManifestPath, meta.Files)
			}
		})
	}
}
