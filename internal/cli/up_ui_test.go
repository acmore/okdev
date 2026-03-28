package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	syncengine "github.com/acmore/okdev/internal/sync"
)

func TestUpUIInteractiveStepRunUpdatesInPlace(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{
		out:         &out,
		errOut:      &out,
		interactive: true,
	}

	ui.stepRun("pod readiness", "Pending 0/2 (ContainerCreating)")
	ui.stepRun("pod readiness", "Running 1/2 (-)")
	ui.stepDone("pod readiness", "ready")

	got := out.String()
	if !strings.Contains(got, "pod readiness: Running 1/2 (-)") {
		t.Fatalf("expected updated transient step message, got %q", got)
	}
	if !strings.Contains(got, "✔ pod readiness: ready\n") {
		t.Fatalf("expected final done line, got %q", got)
	}
	if !strings.Contains(got, "\r\033[K") {
		t.Fatalf("expected transient clear sequence, got %q", got)
	}
}

func TestUpUIWarnWriterAndPrintWarnings(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{out: &out, errOut: &out}

	if _, err := ui.warnWriter().Write([]byte("warning: first warning\nsecond warning\n\n")); err != nil {
		t.Fatal(err)
	}
	ui.printWarnings()

	got := out.String()
	for _, want := range []string{"Warnings:", "- first warning", "- second warning"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected warning output to contain %q: %q", want, got)
		}
	}
}

func TestSyncHelpers(t *testing.T) {
	relPath := "testdata"
	if got := displayLocalSyncPath(relPath); got != filepath.Join(mustAbs("."), relPath) {
		t.Fatalf("unexpected displayed path %q", got)
	}
	if got := syncArrow(""); got != "->" {
		t.Fatalf("unexpected default arrow %q", got)
	}

	cases := map[string]string{
		"up":      "->",
		"down":    "<-",
		"bi":      "<->",
		"invalid": "->",
	}
	for mode, want := range cases {
		if got := modeSymbol(mode); got != want {
			t.Fatalf("unexpected mode symbol for %q: got %q want %q", mode, got, want)
		}
	}
}

func TestSyncPairsSummary(t *testing.T) {
	if got := syncPairsSummary(nil, "->"); got != "no paths" {
		t.Fatalf("unexpected empty summary %q", got)
	}
	pairs := []syncengine.Pair{{Local: ".", Remote: "/remote"}}
	if got := syncPairsSummary(pairs, "->"); !strings.Contains(got, "/remote") || !strings.Contains(got, "->") {
		t.Fatalf("unexpected single-pair summary %q", got)
	}
	pairs = append(pairs, syncengine.Pair{Local: "./two", Remote: "/two"})
	if got := syncPairsSummary(pairs, "->"); got != "2 paths" {
		t.Fatalf("unexpected multi-pair summary %q", got)
	}
}

func TestUpUIPrintReadyCard(t *testing.T) {
	var out bytes.Buffer
	ui := &upUI{out: &out, errOut: &out}

	ui.printReadyCard(
		"sess",
		"default",
		"pod-1",
		"ready",
		"running",
		[]config.PortMapping{
			{Name: "http", Local: 8080, Remote: 80},
			{Name: "api-reverse", Local: 3000, Remote: 3000, Direction: config.PortDirectionReverse},
			{Name: "", Local: 9000, Remote: 9000},
			{Name: "bad", Local: 0, Remote: 80},
		},
		[]syncengine.Pair{{Local: ".", Remote: "/workspace"}},
		"<->",
	)

	got := out.String()
	for _, want := range []string{
		"== Ready ==",
		"session:   sess",
		"sync paths:",
		"<-> /workspace",
		"forwards:",
		"http: localhost:8080 -> remote:80",
		"api-reverse: remote:3000 -> localhost:3000",
		"port: localhost:9000 -> remote:9000",
		"- ssh okdev-sess",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected ready card to contain %q: %q", want, got)
		}
	}
	if strings.Contains(got, "bad") {
		t.Fatalf("did not expect invalid port mapping in output: %q", got)
	}
}

func TestPortMappingSummary(t *testing.T) {
	if got, ok := portMappingSummary(config.PortMapping{Name: "fwd", Local: 8080, Remote: 80}); !ok || got != "fwd: localhost:8080 -> remote:80" {
		t.Fatalf("unexpected forward summary ok=%v got=%q", ok, got)
	}
	if got, ok := portMappingSummary(config.PortMapping{Name: "rev", Local: 3000, Remote: 3000, Direction: config.PortDirectionReverse}); !ok || got != "rev: remote:3000 -> localhost:3000" {
		t.Fatalf("unexpected reverse summary ok=%v got=%q", ok, got)
	}
	if _, ok := portMappingSummary(config.PortMapping{Name: "bad"}); ok {
		t.Fatal("expected invalid mapping to be skipped")
	}
}

func mustAbs(path string) string {
	abs, err := filepath.Abs(path)
	if err != nil {
		panic(err)
	}
	return abs
}
