package sync

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestParsePairsDefault(t *testing.T) {
	pairs, err := ParsePairs(nil, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Local != "." || pairs[0].Remote != "/workspace" {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}
	if pairs[0].Direction != config.SyncDirectionBi {
		t.Fatalf("expected default bi direction, got %q", pairs[0].Direction)
	}
}

func TestParsePairsConfigured(t *testing.T) {
	pairs, err := ParsePairs([]config.SyncPathSpec{
		{Local: ".", Remote: "/workspace"},
		{Local: "./cfg", Remote: "/etc/app", Direction: "down"},
	}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("unexpected pair count: %d", len(pairs))
	}
	// Direction is normalized to the effective value, never empty.
	if pairs[0].Direction != config.SyncDirectionBi || pairs[1].Direction != config.SyncDirectionDown {
		t.Fatalf("unexpected directions: %+v", pairs)
	}
}

func TestParsePairsInvalid(t *testing.T) {
	if _, err := ParsePairs([]config.SyncPathSpec{{Local: "", Remote: "/workspace"}}, "/workspace"); err == nil {
		t.Fatal("expected error")
	}
}

func TestShellEscape(t *testing.T) {
	got := ShellEscape("a'b")
	if got != "'a'\\''b'" {
		t.Fatalf("unexpected escaped shell string %q", got)
	}
}
