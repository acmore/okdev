package ssh

import "testing"

func TestLocalPTYTermUsesCurrentTerm(t *testing.T) {
	t.Setenv("TERM", "xterm-ghostty")
	if got := localPTYTerm(); got != "xterm-ghostty" {
		t.Fatalf("expected current TERM to propagate, got %q", got)
	}
}

func TestLocalPTYTermFallsBackForEmptyOrDumb(t *testing.T) {
	t.Setenv("TERM", "")
	if got := localPTYTerm(); got != "xterm-256color" {
		t.Fatalf("expected fallback TERM for empty value, got %q", got)
	}

	t.Setenv("TERM", "dumb")
	if got := localPTYTerm(); got != "xterm-256color" {
		t.Fatalf("expected fallback TERM for dumb value, got %q", got)
	}
}
