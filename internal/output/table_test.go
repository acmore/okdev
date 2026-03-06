package output

import (
	"bytes"
	"strings"
	"testing"
)

func TestPrintTableEmptyRows(t *testing.T) {
	var b bytes.Buffer
	PrintTable(&b, []string{"NAME", "STATUS"}, nil)
	got := b.String()
	if !strings.Contains(got, "NAME  STATUS") {
		t.Fatalf("missing header row: %q", got)
	}
	if !strings.Contains(got, "----  ------") {
		t.Fatalf("missing separator row: %q", got)
	}
}

func TestPrintTableNormalizesRowWidth(t *testing.T) {
	var b bytes.Buffer
	rows := [][]string{
		{"session-a"},
		{"session-b", "ready", "extra-column-ignored"},
	}
	PrintTable(&b, []string{"NAME", "STATUS"}, rows)
	got := b.String()
	if strings.Contains(got, "extra-column-ignored") {
		t.Fatalf("unexpected extra column in output: %q", got)
	}
	if !strings.Contains(got, "session-a  ") {
		t.Fatalf("missing padded short row: %q", got)
	}
	if !strings.Contains(got, "session-b  ready") {
		t.Fatalf("missing full row: %q", got)
	}
}

func TestPrintTableNoHeaders(t *testing.T) {
	var b bytes.Buffer
	PrintTable(&b, nil, [][]string{{"a"}})
	if b.Len() != 0 {
		t.Fatalf("expected no output when headers are empty, got %q", b.String())
	}
}
