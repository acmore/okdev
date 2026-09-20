package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
)

func TestLoadOptionalConfig(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, config.DefaultFile)
	if err := os.WriteFile(cfgPath, []byte(`
apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: test
spec:
  namespace: team-a
  kubeContext: team-a-cluster
  workload:
    type: pod
    manifestPath: pod.yaml
  sidecar:
    image: ghcr.io/acmore/okdev:edge
`), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}

	opts := &Options{ConfigPath: cfgPath}
	cfg, err := loadOptionalConfig(opts)
	if err != nil {
		t.Fatalf("loadOptionalConfig returned error: %v", err)
	}
	if cfg == nil {
		t.Fatal("expected config")
	}
	if cfg.Spec.Namespace != "team-a" {
		t.Fatalf("expected namespace team-a, got %q", cfg.Spec.Namespace)
	}
	if opts.Context != "team-a-cluster" {
		t.Fatalf("expected the pinned kube context to be applied, got %q", opts.Context)
	}
}

func TestSessionNameFromPodSummary(t *testing.T) {
	if got := sessionNameFromPodSummary(kube.PodSummary{
		Name:   "okdev-demo",
		Labels: map[string]string{"okdev.io/session": "from-label"},
	}); got != "from-label" {
		t.Fatalf("expected label session name, got %q", got)
	}
	if got := sessionNameFromPodSummary(kube.PodSummary{Name: "okdev-demo"}); got != "demo" {
		t.Fatalf("expected okdev- prefix to be trimmed, got %q", got)
	}
	if got := sessionNameFromPodSummary(kube.PodSummary{Name: "plain"}); got != "plain" {
		t.Fatalf("expected plain pod name, got %q", got)
	}
}
