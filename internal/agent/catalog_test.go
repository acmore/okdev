package agent

import "testing"

func TestLookupNormalizesName(t *testing.T) {
	spec, ok := Lookup("  CoDeX ")
	if !ok {
		t.Fatal("expected codex lookup to succeed")
	}
	if spec.Name != "codex" {
		t.Fatalf("expected canonical name codex, got %q", spec.Name)
	}
}

func TestSupportedNamesMatchesCatalog(t *testing.T) {
	names := SupportedNames()
	if len(names) != len(supported) {
		t.Fatalf("expected %d supported names, got %d", len(supported), len(names))
	}
	for _, name := range names {
		if _, ok := supported[name]; !ok {
			t.Fatalf("supported name %q missing from catalog", name)
		}
	}
	if len(names) >= 2 && names[0] > names[1] {
		t.Fatalf("expected sorted names, got %#v", names)
	}
	want := []string{"claude-code", "codex", "gemini", "opencode"}
	if len(names) != len(want) {
		t.Fatalf("expected %#v, got %#v", want, names)
	}
	for i := range want {
		if names[i] != want[i] {
			t.Fatalf("expected %#v, got %#v", want, names)
		}
	}
}
