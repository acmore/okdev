package cli

import (
	"bytes"
	"strings"
	"testing"
)

func TestNewVersionCmdPrintsVersionInfo(t *testing.T) {
	cmd := newVersionCmd()
	var out bytes.Buffer
	cmd.SetOut(&out)
	cmd.SetErr(&out)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("version execute: %v", err)
	}
	got := out.String()
	for _, want := range []string{"okdev ", "commit: ", "date: "} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q, got %q", want, got)
		}
	}
}
