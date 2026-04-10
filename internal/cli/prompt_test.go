package cli

import (
	"bufio"
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

	input := strings.NewReader("./src\n/app\nalice\n")
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

func TestPromptInteractiveUsesExplanatoryLabels(t *testing.T) {
	vars := config.NewTemplateVars()
	input := strings.NewReader("\n\n\n\n\n\n")
	var out bytes.Buffer

	if err := promptInteractive(vars, InitOverrides{}, input, &out, false, true); err != nil {
		t.Fatalf("promptInteractive returned error: %v", err)
	}

	got := out.String()
	for _, want := range []string{
		"Environment name (used for session labels and default naming)",
		"Namespace (where the dev workload will run)",
		"Workload type (pod=simple dev pod, job=batch workload, pytorchjob=distributed training, generic=custom manifest)",
		"Sync local path (project directory on this machine)",
		"Sync remote path (workspace path in the container)",
		"SSH user (login user inside the dev container)",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected prompt output to contain %q, got %q", want, got)
		}
	}
}

func TestSplitCommaList(t *testing.T) {
	got := splitCommaList(" a, ,b ,, c ")
	want := []string{"a", "b", "c"}
	if len(got) != len(want) {
		t.Fatalf("unexpected split result %#v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("unexpected split result %#v", got)
		}
	}
}

func TestPromptWorkloadTypeRejectsInvalidThenAcceptsValid(t *testing.T) {
	in := strings.NewReader("deployment\njob\n")
	reader := bufio.NewReader(in)
	var out bytes.Buffer
	got, err := promptWorkloadType(reader, &out, "pod")
	if err != nil {
		t.Fatalf("promptWorkloadType error: %v", err)
	}
	if got != "job" {
		t.Fatalf("expected job, got %q", got)
	}
	if !strings.Contains(out.String(), "invalid workload type") {
		t.Fatalf("expected invalid-workload warning, got %q", out.String())
	}
}
