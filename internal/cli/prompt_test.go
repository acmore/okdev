package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestApplyFlagOverrides(t *testing.T) {
	vars := config.NewTemplateVars()
	overrides := InitOverrides{
		Name:         "my-env",
		Namespace:    "staging",
		SidecarImage: "ghcr.io/acmore/okdev:test",
		SyncLocal:    "./local",
		SyncRemote:   "/remote",
		SSHUser:      "devuser",
	}
	applyOverrides(vars, overrides)

	if vars.Name != "my-env" {
		t.Fatalf("expected name my-env, got %q", vars.Name)
	}
	if vars.Namespace != "staging" {
		t.Fatalf("expected namespace staging, got %q", vars.Namespace)
	}
	if vars.SidecarImage != "ghcr.io/acmore/okdev:test" {
		t.Fatalf("expected sidecar image override, got %q", vars.SidecarImage)
	}
	if vars.SyncLocal != "./local" {
		t.Fatalf("expected sync local override, got %q", vars.SyncLocal)
	}
	if vars.SyncRemote != "/remote" {
		t.Fatalf("expected sync remote override, got %q", vars.SyncRemote)
	}
	if vars.SSHUser != "devuser" {
		t.Fatalf("expected ssh user override, got %q", vars.SSHUser)
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

func TestPromptInteractiveRequiresTTYUnlessYes(t *testing.T) {
	vars := config.NewTemplateVars()
	err := promptInteractive(vars, InitOverrides{}, strings.NewReader(""), &bytes.Buffer{}, false, false)
	if err == nil {
		t.Fatal("expected non-interactive prompt attempt to fail without --yes")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected error to mention --yes, got %v", err)
	}
}

func TestPromptInteractiveSkipsOverriddenFields(t *testing.T) {
	vars := config.NewTemplateVars()
	overrides := InitOverrides{
		Name:         "flag-name",
		Namespace:    "flag-ns",
		WorkloadType: "pod",
	}
	applyOverrides(vars, overrides)

	input := strings.NewReader("custom-image\n./src\n/app\nalice\n")
	var out bytes.Buffer
	if err := promptInteractive(vars, overrides, input, &out, false, true); err != nil {
		t.Fatalf("promptInteractive returned error: %v", err)
	}

	if vars.Name != "flag-name" {
		t.Fatalf("expected overridden name to remain unchanged, got %q", vars.Name)
	}
	if vars.Namespace != "flag-ns" {
		t.Fatalf("expected overridden namespace to remain unchanged, got %q", vars.Namespace)
	}
	if vars.SidecarImage != "custom-image" {
		t.Fatalf("expected prompted sidecar image, got %q", vars.SidecarImage)
	}
	if vars.SyncLocal != "./src" {
		t.Fatalf("expected prompted sync local, got %q", vars.SyncLocal)
	}
	if vars.SyncRemote != "/app" {
		t.Fatalf("expected prompted sync remote, got %q", vars.SyncRemote)
	}
	if vars.SSHUser != "alice" {
		t.Fatalf("expected prompted ssh user, got %q", vars.SSHUser)
	}
	if strings.Contains(out.String(), "Environment name") || strings.Contains(out.String(), "Namespace") {
		t.Fatalf("expected overridden fields to be skipped, got prompts:\n%s", out.String())
	}
}
