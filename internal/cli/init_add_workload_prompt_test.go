package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

// nonInteractiveVarResolver resolves declared variables from --set and
// defaults without prompting, which is what --yes does in the real command.
func nonInteractiveVarResolver(sets map[string]string) templateVarResolver {
	return func(meta *config.TemplateMeta) (map[string]any, error) {
		return resolveInitTemplateVars(meta, sets, nil, true, false, strings.NewReader(""), io.Discard)
	}
}

// runInitWithStdin drives init with a reader that is deliberately not a
// terminal, so the non-interactive path is exercised deterministically rather
// than depending on whatever stdin the test binary inherited.
func runInitWithStdin(t *testing.T, dir string, stdin io.Reader, args ...string) (string, error) {
	t.Helper()
	cmd, _ := newRootCmdWithOptions()
	var out strings.Builder
	cmd.SetOut(&out)
	cmd.SetErr(&out)
	cmd.SetIn(stdin)
	cmd.SetArgs(append([]string{"init"}, args...))
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(dir); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = os.Chdir(wd) }()
	return out.String(), cmd.Execute()
}

// Adding a workload instantiates a template, exactly as creating a config
// does, so its variables get the same prompt. They were resolved
// non-interactively instead, which silently took every default — the same
// reasoning that put --set on the template side rather than the project side
// applies to asking for those values.
func TestPlanWorkloadAdditionPromptsForTemplateVariables(t *testing.T) {
	dir := t.TempDir()
	writeVarTemplate(t, dir)

	var prompts bytes.Buffer
	answers := strings.NewReader("custom.yaml\n5\n")
	resolve := func(meta *config.TemplateMeta) (map[string]any, error) {
		return resolveInitTemplateVars(meta, nil, nil, false, true, answers, &prompts)
	}

	cfgPath := filepath.Join(dir, ".okdev", "okdev.yaml")
	cfg, _, err := config.LoadFromBytes([]byte(podOnlyConfig), cfgPath)
	if err != nil {
		t.Fatalf("load fixture config: %v", err)
	}
	add, err := planWorkloadAddition(cfgPath, []byte(podOnlyConfig), cfg,
		config.NewTemplateVars(), nil, resolve, "train", "trainer", dir)
	if err != nil {
		t.Fatalf("planWorkloadAddition: %v", err)
	}

	if !strings.Contains(prompts.String(), "workerReplicas") {
		t.Fatalf("expected a prompt naming the variable, got:\n%s", prompts.String())
	}
	// The default must be offered as a hint, not silently applied.
	if !strings.Contains(prompts.String(), "(1)") {
		t.Fatalf("expected the declared default shown as a hint, got:\n%s", prompts.String())
	}
	if !strings.Contains(string(add.ManifestBytes), "replicas: 5") {
		t.Fatalf("the answered value must reach the manifest:\n%s", add.ManifestBytes)
	}
}

// Without a TTY and without --yes there is nobody to answer, so it must say so
// rather than quietly pick defaults — the same refusal a fresh init gives.
func TestInitAdditiveRequiresYesWithoutATTY(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir, "--yes", "--name", "proj", "--namespace", "default"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	writeVarTemplate(t, dir)

	_, err := runInitWithStdin(t, dir, strings.NewReader(""),
		"--template", "trainer", "--workload-name", "train")
	if err == nil {
		t.Fatal("expected a refusal when no TTY and no --yes")
	}
	if !strings.Contains(err.Error(), "--yes") || !strings.Contains(err.Error(), "--set") {
		t.Fatalf("the refusal must name the way out, got: %v", err)
	}
}

// --yes keeps working headlessly: --set wins, declared defaults fill the rest.
func TestInitAdditiveWithYesStaysNonInteractive(t *testing.T) {
	dir := t.TempDir()
	if _, err := runInit(t, dir, "--yes", "--name", "proj", "--namespace", "default"); err != nil {
		t.Fatalf("first init: %v", err)
	}
	writeVarTemplate(t, dir)

	if _, err := runInitWithStdin(t, dir, strings.NewReader(""), "--yes",
		"--template", "trainer", "--workload-name", "train", "--set", "workerReplicas=6"); err != nil {
		t.Fatalf("additive init with --yes: %v", err)
	}
	manifest, err := os.ReadFile(filepath.Join(dir, ".okdev", "train.yaml"))
	if err != nil {
		t.Fatalf("manifest must be scaffolded: %v", err)
	}
	if !strings.Contains(string(manifest), "replicas: 6") {
		t.Fatalf("--set must still win under --yes:\n%s", manifest)
	}
}
