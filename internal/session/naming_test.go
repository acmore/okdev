package session

import "testing"

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
