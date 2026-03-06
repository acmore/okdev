package cli

import "testing"

func TestSyncPairsDefault(t *testing.T) {
	pairs, err := syncPairs(nil, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 1 || pairs[0].Local != "." || pairs[0].Remote != "/workspace" {
		t.Fatalf("unexpected pairs: %+v", pairs)
	}
}

func TestSyncPairsConfigured(t *testing.T) {
	pairs, err := syncPairs([]string{".:/workspace", "./cfg:/etc/app"}, "/workspace")
	if err != nil {
		t.Fatal(err)
	}
	if len(pairs) != 2 {
		t.Fatalf("unexpected pair count: %d", len(pairs))
	}
}

func TestSyncPairsInvalid(t *testing.T) {
	if _, err := syncPairs([]string{"invalid"}, "/workspace"); err == nil {
		t.Fatal("expected error")
	}
}

func TestShellEscape(t *testing.T) {
	got := shellEscape("a'b")
	if got != "'a'\\''b'" {
		t.Fatalf("unexpected escaped shell string %q", got)
	}
}
