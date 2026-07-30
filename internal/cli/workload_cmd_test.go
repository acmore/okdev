package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/session"
)

func TestResolveWorkloadProfileNamePrefersFlagOverPin(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := session.SaveWorkloadProfile("sess1", "train"); err != nil {
		t.Fatal(err)
	}

	got, err := resolveWorkloadProfileName(&Options{Workload: "dev"}, "sess1")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "dev" {
		t.Fatalf("with --workload dev, got %q", got)
	}

	got, err = resolveWorkloadProfileName(&Options{}, "sess1")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "train" {
		t.Fatalf("without a flag the pin must win, got %q", got)
	}
}

func TestResolveWorkloadProfileNameEmptyWhenUnpinned(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	got, err := resolveWorkloadProfileName(&Options{}, "fresh")
	if err != nil {
		t.Fatalf("resolveWorkloadProfileName: %v", err)
	}
	if got != "" {
		t.Fatalf("an unpinned session must resolve to \"\", got %q", got)
	}
}
