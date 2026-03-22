package config

import (
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRunMigrationsNoOp(t *testing.T) {
	input := "apiVersion: okdev.io/v1alpha1\nkind: DevEnvironment\nmetadata:\n  name: test\n"
	var doc yaml.Node
	if err := yaml.Unmarshal([]byte(input), &doc); err != nil {
		t.Fatal(err)
	}

	result, err := RunMigrations(&doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Applied) != 0 {
		t.Fatalf("expected 0 applied migrations, got %d", len(result.Applied))
	}
	if len(result.Warnings) != 0 {
		t.Fatalf("expected 0 warnings, got %d", len(result.Warnings))
	}
}
