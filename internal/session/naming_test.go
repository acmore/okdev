package session

import (
	"strings"
	"testing"
)

func TestSanitize(t *testing.T) {
	got := sanitize("Repo/Feature_Branch")
	if got != "repo-feature-branch" {
		t.Fatalf("unexpected sanitize result %q", got)
	}
}

func TestResolveExplicit(t *testing.T) {
	name, err := Resolve("My_Session", "")
	if err != nil {
		t.Fatal(err)
	}
	if name != "my-session" {
		t.Fatalf("unexpected resolved name %q", name)
	}
}

func TestSanitizeEmptyFallsBack(t *testing.T) {
	if got := sanitize("___"); got != "okdev-session" {
		t.Fatalf("unexpected sanitize fallback %q", got)
	}
}

func TestSanitizeTruncatesLongNames(t *testing.T) {
	got := sanitize(strings.Repeat("ab", 30))
	if len(got) > 50 {
		t.Fatalf("expected truncated name, got len=%d name=%q", len(got), got)
	}
}

func TestValidateNonEmptyAllowsSanitizedName(t *testing.T) {
	if err := ValidateNonEmpty("My Session"); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}
