package cli

import (
	"reflect"
	"testing"
)

func TestParsePortForwardMappings(t *testing.T) {
	got, err := parsePortForwardMappings([]string{"8080:8080", "9000:9001"})
	if err != nil {
		t.Fatalf("parsePortForwardMappings: %v", err)
	}
	want := []string{"8080:8080", "9000:9001"}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected mappings: got=%v want=%v", got, want)
	}
}

func TestParsePortForwardMappingsRejectsInvalidValues(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{},
		{"8080"},
		{"abc:8080"},
		{"8080:def"},
		{"0:8080"},
		{"8080:0"},
		{"8080:8080:8080"},
	} {
		if _, err := parsePortForwardMappings(tc); err == nil {
			t.Fatalf("expected error for %v", tc)
		}
	}
}
