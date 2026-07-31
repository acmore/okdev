package session

import "testing"

func TestWorkloadProfilePinRoundTrip(t *testing.T) {
	t.Setenv("HOME", t.TempDir())

	got, err := LoadWorkloadProfile("sess1")
	if err != nil {
		t.Fatalf("LoadWorkloadProfile on a fresh session: %v", err)
	}
	if got != "" {
		t.Fatalf("fresh session pin = %q, want empty", got)
	}

	if err := SaveWorkloadProfile("sess1", "train"); err != nil {
		t.Fatalf("SaveWorkloadProfile: %v", err)
	}
	got, err = LoadWorkloadProfile("sess1")
	if err != nil {
		t.Fatalf("LoadWorkloadProfile: %v", err)
	}
	if got != "train" {
		t.Fatalf("pin = %q, want train", got)
	}

	if err := ClearWorkloadProfile("sess1"); err != nil {
		t.Fatalf("ClearWorkloadProfile: %v", err)
	}
	got, _ = LoadWorkloadProfile("sess1")
	if got != "" {
		t.Fatalf("pin after clear = %q, want empty", got)
	}
}

func TestWorkloadProfilePinIsPerSession(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	if err := SaveWorkloadProfile("a", "train"); err != nil {
		t.Fatal(err)
	}
	if err := SaveWorkloadProfile("b", "dev"); err != nil {
		t.Fatal(err)
	}
	if got, _ := LoadWorkloadProfile("a"); got != "train" {
		t.Fatalf("session a pin = %q, want train", got)
	}
	if got, _ := LoadWorkloadProfile("b"); got != "dev" {
		t.Fatalf("session b pin = %q, want dev", got)
	}
}

func TestLoadWorkloadProfileEmptySessionName(t *testing.T) {
	if got, err := LoadWorkloadProfile("  "); err != nil || got != "" {
		t.Fatalf("LoadWorkloadProfile(\"\") = %q, %v; want \"\", nil", got, err)
	}
}
