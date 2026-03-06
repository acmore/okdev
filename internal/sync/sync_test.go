package sync

import "testing"

func TestParsePairsDefault(t *testing.T) {
	pairs, err := ParsePairs(nil, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Local != "." || pairs[0].Remote != "/workspace" {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}
}

func TestParsePairsConfigured(t *testing.T) {
	pairs, err := ParsePairs([]string{".:/workspace", "./cfg:/etc/app"}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("unexpected pair count: %d", len(pairs))
	}
}

func TestParsePairsInvalid(t *testing.T) {
	if _, err := ParsePairs([]string{"invalid"}, "/workspace"); err == nil {
		t.Fatal("expected error")
	}
}

func TestShellEscape(t *testing.T) {
	got := ShellEscape("a'b")
	if got != "'a'\\''b'" {
		t.Fatalf("unexpected escaped shell string %q", got)
	}
}
