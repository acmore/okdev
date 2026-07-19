package config

import (
	"strings"
	"testing"
)

func TestUnknownSpecFieldWarnings(t *testing.T) {
	raw := []byte(`
apiVersion: okdev.dev/v1
kind: DevEnvironment
metadata:
  name: x
spec:
  context: my-cluster
  kubeContext: real-cluster
  session:
    defaultNameTemplate: "{{ .Repo }}"
    ttlHours: 72
  sync:
    engine: syncthing
`)
	warnings := UnknownSpecFieldWarnings(raw)
	joined := strings.Join(warnings, "\n")
	if !strings.Contains(joined, "spec.context is not a recognized field") {
		t.Fatalf("expected typo warning for spec.context, got: %v", warnings)
	}
	if !strings.Contains(joined, "kubeContext") {
		t.Fatalf("typo warning should list known keys including kubeContext, got: %v", warnings)
	}
	if !strings.Contains(joined, "spec.session.ttlHours is ignored") || !strings.Contains(joined, "no longer expire") {
		t.Fatalf("expected removed-field hint for ttlHours, got: %v", warnings)
	}
	for _, unexpected := range []string{"spec.kubeContext ", "defaultNameTemplate", "spec.sync.engine"} {
		if strings.Contains(joined, unexpected) {
			t.Fatalf("valid field %q must not warn: %v", unexpected, warnings)
		}
	}
}

func TestUnknownSpecFieldWarningsOpaqueSections(t *testing.T) {
	raw := []byte(`
spec:
  podTemplate:
    spec:
      totallyMadeUp: true
`)
	if warnings := UnknownSpecFieldWarnings(raw); len(warnings) != 0 {
		t.Fatalf("external-typed sections must stay opaque, got: %v", warnings)
	}
}

func TestUnknownSpecFieldWarningsCleanConfig(t *testing.T) {
	raw := []byte(`
spec:
  kubeContext: prod
  sync:
    engine: syncthing
`)
	if warnings := UnknownSpecFieldWarnings(raw); len(warnings) != 0 {
		t.Fatalf("clean config must not warn, got: %v", warnings)
	}
}
