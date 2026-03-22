package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestApplyFlagOverrides(t *testing.T) {
	vars := config.NewTemplateVars()
	overrides := InitOverrides{
		Name:      "my-env",
		Namespace: "staging",
	}
	applyOverrides(vars, overrides)

	if vars.Name != "my-env" {
		t.Fatalf("expected name my-env, got %q", vars.Name)
	}
	if vars.Namespace != "staging" {
		t.Fatalf("expected namespace staging, got %q", vars.Namespace)
	}
}

func TestApplyFlagOverridesEmptyNoChange(t *testing.T) {
	vars := config.NewTemplateVars()
	vars.Name = "original"
	overrides := InitOverrides{}
	applyOverrides(vars, overrides)

	if vars.Name != "original" {
		t.Fatalf("expected name to remain original, got %q", vars.Name)
	}
}
