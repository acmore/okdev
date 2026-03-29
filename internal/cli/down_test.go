package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewDownCmdDeprecatesDeletePVC(t *testing.T) {
	cmd := newDownCmd(&Options{})
	flag := cmd.Flags().Lookup("delete-pvc")
	if flag == nil {
		t.Fatal("expected delete-pvc flag")
	}
	if flag.Deprecated == "" {
		t.Fatal("expected delete-pvc to be marked deprecated")
	}
}

func TestNewDownCmdHasYesFlag(t *testing.T) {
	cmd := newDownCmd(&Options{})
	flag := cmd.Flags().Lookup("yes")
	if flag == nil {
		t.Fatal("expected yes flag")
	}
	if flag.Shorthand != "y" {
		t.Fatalf("expected shorthand -y, got %q", flag.Shorthand)
	}
}

func TestPromptConfirmDownAccepts(t *testing.T) {
	for _, input := range []string{"y\n", "Y\n", "yes\n", "YES\n", " y \n"} {
		in := strings.NewReader(input)
		var out bytes.Buffer
		ok, err := promptConfirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", input, err)
		}
		if !ok {
			t.Fatalf("input %q: expected confirmation", input)
		}
		if !strings.Contains(out.String(), "my-session") {
			t.Fatalf("expected prompt to contain session name, got %q", out.String())
		}
	}
}

func TestPromptConfirmDownRejects(t *testing.T) {
	for _, input := range []string{"n\n", "N\n", "no\n", "\n", "maybe\n"} {
		in := strings.NewReader(input)
		var out bytes.Buffer
		ok, err := promptConfirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
		if err != nil {
			t.Fatalf("input %q: unexpected error: %v", input, err)
		}
		if ok {
			t.Fatalf("input %q: expected rejection", input)
		}
	}
}

func TestConfirmDownRejectsNonTTY(t *testing.T) {
	in := strings.NewReader("y\n")
	var out bytes.Buffer
	_, err := confirmDown(in, &out, "my-session", "default", "Pod", "okdev-my-session")
	if err == nil {
		t.Fatal("expected error for non-TTY input")
	}
	if !strings.Contains(err.Error(), "--yes") {
		t.Fatalf("expected error to mention --yes, got %q", err.Error())
	}
}
