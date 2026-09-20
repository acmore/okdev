package cli

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/session"
)

func writeSelectionConfig(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	raw := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: demo\nspec:\n  namespace: default\n  session:\n    defaultNameTemplate: fresh-session\n  workload:\n    type: pod\n    manifestPath: pod.yaml\n"
	if err := os.WriteFile(path, []byte(raw), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNamedSessionDiscoversConfigBeforeFailing(t *testing.T) {
	for _, saved := range []bool{false, true} {
		name := "new session"
		if saved {
			name = "legacy session without config path"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OKDEV_CONFIG", "")
			repo := t.TempDir()
			t.Chdir(repo)
			path := filepath.Join(repo, ".okdev", "okdev.yaml")
			writeSelectionConfig(t, path)
			if saved {
				if err := session.SaveInfo(session.Info{Name: "new-session"}); err != nil {
					t.Fatal(err)
				}
			}
			opts := &Options{Session: "new-session"}
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				t.Fatal(err)
			}
			if cc.cfgPath != path || cc.sessionName != "new-session" {
				t.Fatalf("resolved config/session = %q/%q, want %q/new-session", cc.cfgPath, cc.sessionName, path)
			}
			if opts.ConfigPath != "" {
				t.Fatal("mutated caller options")
			}
		})
	}
}

func TestNamedSessionConfigPrecedence(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OKDEV_CONFIG", "")
	repo := t.TempDir()
	t.Chdir(repo)
	discovered := filepath.Join(repo, ".okdev", "okdev.yaml")
	saved := filepath.Join(repo, "saved", "okdev.yaml")
	explicit := filepath.Join(repo, "explicit", "okdev.yaml")
	for _, path := range []string{discovered, saved, explicit} {
		writeSelectionConfig(t, path)
	}
	if err := session.SaveInfo(session.Info{Name: "existing", ConfigPath: saved}); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, flag, want string }{
		{"saved wins discovery", "", saved}, {"explicit wins saved", explicit, explicit},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cc, err := resolveCommandContext(&Options{Session: "existing", ConfigPath: tc.flag}, resolveSessionName)
			if err != nil {
				t.Fatal(err)
			}
			if cc.cfgPath != tc.want {
				t.Fatalf("config = %q, want %q", cc.cfgPath, tc.want)
			}
		})
	}
}

func TestExplicitConfigNeverInfersUnrelatedSession(t *testing.T) {
	for _, source := range []string{"active", "pod", "controller", "saved workload"} {
		for _, association := range []string{"other config", "missing path", "missing metadata", "matching config"} {
			t.Run(source+"/"+association, func(t *testing.T) {
				t.Setenv("HOME", t.TempDir())
				t.Setenv("OKDEV_CONFIG", "")
				repo := t.TempDir()
				t.Chdir(repo)
				path := filepath.Join(repo, ".okdev", "environment-b", "okdev.yaml")
				writeSelectionConfig(t, path)
				repoRoot, err := session.RepoRoot()
				if err != nil {
					t.Fatal(err)
				}
				info := session.Info{Name: "session-a", RepoRoot: repoRoot, Namespace: "default", WorkloadAPIVersion: "batch/v1", WorkloadKind: "Job", WorkloadName: "trainer"}
				switch association {
				case "other config":
					info.ConfigPath = filepath.Join(repo, ".okdev", "environment-a", "okdev.yaml")
				case "matching config":
					info.ConfigPath = path
				}
				if association != "missing metadata" {
					if err := session.SaveInfo(info); err != nil {
						t.Fatal(err)
					}
				}
				labels := map[string]string{"okdev.io/managed": "true", "okdev.io/session": "session-a", "okdev.io/repo": filepath.Base(repo), "okdev.io/name": "demo"}
				reader := fakeSessionAccessReader{}
				switch source {
				case "active", "pod":
					reader.pods = []kube.PodSummary{{Name: "okdev-session-a", Labels: labels}}
				case "controller":
					reader.resources = []kube.ResourceSummary{{Name: "trainer", Kind: "Job", APIVersion: "batch/v1", Labels: labels}}
				case "saved workload":
					reader.resourceExists = true
				}
				if source == "active" {
					if err := session.SaveActiveSession("session-a"); err != nil {
						t.Fatal(err)
					}
				}
				cc, err := resolveCommandContext(&Options{ConfigPath: path}, func(opts *Options, cfg *config.DevEnvironment, ns string) (string, error) {
					return resolveSessionNameWithReader(opts, cfg, ns, true, reader)
				})
				if err != nil {
					t.Fatal(err)
				}
				want := "fresh-session"
				if association == "matching config" {
					want = "session-a"
				}
				if cc.sessionName != want {
					t.Fatalf("session = %q, want %q", cc.sessionName, want)
				}
				if source == "active" {
					active, err := session.LoadActiveSession()
					if err != nil || active != "session-a" {
						t.Fatalf("unrelated active session changed: %q, %v", active, err)
					}
				}
			})
		}
	}
}

func TestExplicitConfigDefaultCannotCollideWithUnassociatedLiveSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OKDEV_CONFIG", "")
	path := filepath.Join(t.TempDir(), "okdev.yaml")
	writeSelectionConfig(t, path)
	reader := fakeSessionAccessReader{pods: []kube.PodSummary{{Name: "okdev-fresh-session", Labels: map[string]string{"okdev.io/session": "fresh-session"}}}}
	_, err := resolveCommandContext(&Options{ConfigPath: path}, func(opts *Options, cfg *config.DevEnvironment, ns string) (string, error) {
		return resolveSessionNameWithReader(opts, cfg, ns, true, reader)
	})
	if err == nil {
		t.Fatal("must not silently reuse a live session with an unknown config association")
	}
	for _, want := range []string{"fresh-session", path, "--session"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q lacks %q", err, want)
		}
	}
	cc, err := resolveCommandContext(&Options{ConfigPath: path, Session: "fresh-session"}, resolveSessionName)
	if err != nil || cc.sessionName != "fresh-session" {
		t.Fatalf("explicit session must remain usable: %v", err)
	}
}

func TestExplicitConfigFindsPendingControllerPastUnrelatedPods(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("OKDEV_CONFIG", "")
	path := filepath.Join(t.TempDir(), "okdev.yaml")
	writeSelectionConfig(t, path)
	if err := session.SaveInfo(session.Info{Name: "pending", ConfigPath: path}); err != nil {
		t.Fatal(err)
	}
	reader := fakeSessionAccessReader{
		pods:      []kube.PodSummary{{Name: "okdev-unrelated", Labels: map[string]string{"okdev.io/session": "unrelated"}}},
		resources: []kube.ResourceSummary{{Name: "trainer", Kind: "Job", APIVersion: "batch/v1", Labels: map[string]string{"okdev.io/session": "pending"}}},
	}
	cc, err := resolveCommandContext(&Options{ConfigPath: path}, func(opts *Options, cfg *config.DevEnvironment, ns string) (string, error) {
		return resolveSessionNameWithReader(opts, cfg, ns, true, reader)
	})
	if err != nil {
		t.Fatal(err)
	}
	if cc.sessionName != "pending" {
		t.Fatalf("session = %q, want pending", cc.sessionName)
	}
}

type inferenceHiddenSessionReader struct{ fakeSessionAccessReader }

func (r inferenceHiddenSessionReader) ListPods(ctx context.Context, ns string, all bool, selector string) ([]kube.PodSummary, error) {
	if strings.Contains(selector, "okdev.io/name=") {
		return nil, nil
	}
	return r.fakeSessionAccessReader.ListPods(ctx, ns, all, selector)
}

func (r inferenceHiddenSessionReader) ListResources(ctx context.Context, ns string, all bool, api, kind, selector string) ([]kube.ResourceSummary, error) {
	if strings.Contains(selector, "okdev.io/name=") {
		return nil, nil
	}
	return r.fakeSessionAccessReader.ListResources(ctx, ns, all, api, kind, selector)
}

func TestExplicitConfigRejectsDefaultCollisionOutsideInferenceLabels(t *testing.T) {
	for _, kind := range []string{"pod", "controller"} {
		t.Run(kind, func(t *testing.T) {
			t.Setenv("HOME", t.TempDir())
			t.Setenv("OKDEV_CONFIG", "")
			path := filepath.Join(t.TempDir(), "okdev.yaml")
			writeSelectionConfig(t, path)
			labels := map[string]string{"okdev.io/session": "fresh-session", "okdev.io/name": "other-config"}
			reader := inferenceHiddenSessionReader{}
			if kind == "pod" {
				reader.pods = []kube.PodSummary{{Name: "okdev-fresh-session", Labels: labels}}
			} else {
				reader.resources = []kube.ResourceSummary{{Name: "trainer", Kind: "Job", APIVersion: "batch/v1", Labels: labels}}
			}
			_, err := resolveCommandContext(&Options{ConfigPath: path}, func(opts *Options, cfg *config.DevEnvironment, ns string) (string, error) {
				return resolveSessionNameWithReader(opts, cfg, ns, true, reader)
			})
			if err == nil {
				t.Fatal("default session must not reuse a live workload excluded by config-name labels")
			}
			for _, want := range []string{"fresh-session", path, "--session"} {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q lacks %q", err, want)
				}
			}
		})
	}
}
