package cli

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
	"github.com/spf13/cobra"
)

func TestDriftResultNoAnnotation(t *testing.T) {
	current := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:1"}
	result := detectDrift(&current, "", "")
	if result.Kind != driftUnknown {
		t.Fatalf("expected driftUnknown, got %v", result.Kind)
	}
}

func TestDriftResultNoDrift(t *testing.T) {
	snap := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:1"}
	hash, _ := snap.SHA256()
	jsonStr, _ := snap.JSON()
	result := detectDrift(&snap, jsonStr, hash)
	if result.Kind != driftNone {
		t.Fatalf("expected driftNone, got %v", result.Kind)
	}
}

func TestDriftResultChanged(t *testing.T) {
	old := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:1"}
	oldJSON, _ := old.JSON()
	oldHash, _ := old.SHA256()

	current := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:2"}
	result := detectDrift(&current, oldJSON, oldHash)
	if result.Kind != driftChanged {
		t.Fatalf("expected driftChanged, got %v", result.Kind)
	}
	if result.Diff == "" {
		t.Fatal("expected non-empty diff")
	}
}

func TestDriftResultChangedMalformedJSON(t *testing.T) {
	current := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:2"}
	result := detectDrift(&current, "not-json", "different-hash")
	if result.Kind != driftChanged {
		t.Fatalf("expected driftChanged, got %v", result.Kind)
	}
	if result.Diff != "" {
		t.Fatal("expected empty diff when old JSON is malformed")
	}
}

func TestRenderDiffOutput(t *testing.T) {
	old := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:1"}
	new := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:2"}
	diff := renderSpecDiff(&old, &new)
	if !strings.Contains(diff, "-") || !strings.Contains(diff, "+") {
		t.Fatalf("expected unified diff markers, got:\n%s", diff)
	}
	if !strings.Contains(diff, "img:1") || !strings.Contains(diff, "img:2") {
		t.Fatalf("expected image change in diff, got:\n%s", diff)
	}
}

func TestConfirmDriftReapplyAccepts(t *testing.T) {
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	ok, err := promptDriftReapply(in, &out, "Reapply workload? [y/N]: ")
	if err != nil {
		t.Fatal(err)
	}
	if !ok {
		t.Fatal("expected confirmation")
	}
}

func TestConfirmDriftReapplyDeclines(t *testing.T) {
	in := strings.NewReader("n\n")
	var out bytes.Buffer
	ok, err := promptDriftReapply(in, &out, "Reapply workload? [y/N]: ")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected decline")
	}
}

func TestConfirmDriftReapplyDefaultsNo(t *testing.T) {
	in := strings.NewReader("\n")
	var out bytes.Buffer
	ok, err := promptDriftReapply(in, &out, "Reapply workload? [y/N]: ")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected default decline")
	}
}

func TestDriftDetectionMissingAnnotationWarns(t *testing.T) {
	snap := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:1"}
	result := detectDrift(&snap, "", "")
	if result.Kind != driftUnknown {
		t.Fatalf("expected driftUnknown for missing annotation, got %v", result.Kind)
	}
}

func TestDriftDetectionHashOnlyNoJSON(t *testing.T) {
	current := config.LastAppliedWorkloadSpec{Version: "v1", WorkloadKind: "pod", SidecarImage: "img:2"}
	result := detectDrift(&current, "", "some-old-hash")
	if result.Kind != driftChanged {
		t.Fatalf("expected driftChanged when hash present but different, got %v", result.Kind)
	}
	if result.Diff != "" {
		t.Fatal("expected empty diff when no old JSON available")
	}
}

func TestUnifiedDiffEmptyInputs(t *testing.T) {
	result := unifiedDiff("", "")
	if result != "" {
		t.Fatalf("expected empty diff for empty inputs, got %q", result)
	}
}

func TestUnifiedDiffIdentical(t *testing.T) {
	input := "line1\nline2\n"
	result := unifiedDiff(input, input)
	if strings.Contains(result, "+ ") || strings.Contains(result, "- ") {
		t.Fatalf("expected no changes in diff for identical input, got:\n%s", result)
	}
}

func TestHandleChangedWorkloadDriftStopsActiveStatusBeforePrompt(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{
		out:         &out,
		errOut:      &out,
		interactive: true,
	}
	ui.stepRun("job", "trainer")

	state := &upState{
		ctx:     context.Background(),
		cmd:     testCommandWithIO(strings.NewReader("n\n"), &out, &out),
		ui:      ui,
		runtime: &fakeRefRuntime{kind: workload.TypeGeneric, apiVersion: "apps/v1", name: "trainer"},
	}

	action, err := handleChangedWorkloadDrift(state, "diff-body", false, true)
	if err != nil {
		t.Fatalf("handleChangedWorkloadDrift: %v", err)
	}
	if action != driftActionReuse {
		t.Fatalf("expected reuse on decline, got %v", action)
	}
	if ui.active != nil || ui.activeStep != "" {
		t.Fatalf("expected active status to be stopped before prompting")
	}
	got := out.String()
	if !strings.Contains(got, "\r\033[K") {
		t.Fatalf("expected transient status clear before prompt, got %q", got)
	}
	if !strings.Contains(got, "Reapply workload? [y/N]: ") {
		t.Fatalf("expected reapply prompt, got %q", got)
	}
}

func TestHandleChangedWorkloadDriftPromptsRecreateForImmutableController(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{
		out:         &out,
		errOut:      &out,
		interactive: true,
	}
	ui.stepRun("job", "trainer")

	state := &upState{
		ctx: context.Background(),
		cmd: testCommandWithIO(strings.NewReader("n\n"), &out, &out),
		ui:  ui,
		runtime: &fakeRefRuntime{
			kind:       workload.TypeJob,
			apiVersion: "batch/v1",
			name:       "trainer",
		},
		command: &commandContext{sessionName: "sess"},
	}

	action, err := handleChangedWorkloadDrift(state, "diff-body", false, true)
	if err != nil {
		t.Fatalf("handleChangedWorkloadDrift: %v", err)
	}
	if action != driftActionReuse {
		t.Fatalf("expected reuse on decline, got %v", action)
	}
	got := out.String()
	if !strings.Contains(got, "This workload must be recreated to apply these changes.") {
		t.Fatalf("expected recreate guidance, got %q", got)
	}
	if !strings.Contains(got, "Recreate workload? [y/N]: ") {
		t.Fatalf("expected recreate prompt, got %q", got)
	}
}

func TestHandleChangedWorkloadDriftShowsWaitStatusDuringRecreate(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{
		out:         &out,
		errOut:      &out,
		interactive: false,
	}

	k := &fakeWorkloadExistenceChecker{
		existsSeq: []bool{true, true, false, false},
		podsSeq: [][]kube.PodSummary{
			{{Name: "trainer-old-0"}},
			{{Name: "trainer-old-0"}},
			{},
		},
	}

	state := &upState{
		ctx:           context.Background(),
		cmd:           testCommandWithIO(strings.NewReader("y\n"), &out, &out),
		ui:            ui,
		reconcileKube: k,
		runtime: &fakeRefRuntime{
			kind:       workload.TypeJob,
			apiVersion: "batch/v1",
			name:       "trainer",
		},
		command: &commandContext{
			namespace:   "default",
			sessionName: "sess",
			kube:        &kube.Client{},
		},
		flags: upOptions{
			waitTimeout: 3 * time.Second,
		},
	}

	action, err := handleChangedWorkloadDrift(state, "diff-body", false, true)
	if err != nil {
		t.Fatalf("handleChangedWorkloadDrift: %v", err)
	}
	if action != driftActionRecreate {
		t.Fatalf("expected recreate action, got %v", action)
	}

	got := out.String()
	for _, want := range []string{
		"Recreate workload? [y/N]: ",
		"… reconcile: deleting existing workload",
		"… reconcile: waiting for workload batch/v1/job/trainer to be deleted",
		"… reconcile: waiting for 1 old session pod to terminate",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got %q", want, got)
		}
	}
}

func testCommandWithIO(in io.Reader, out, errOut io.Writer) *cobra.Command {
	cmd := &cobra.Command{}
	cmd.SetIn(in)
	cmd.SetOut(out)
	cmd.SetErr(errOut)
	return cmd
}
