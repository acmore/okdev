package sidecar

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	goDirectivePattern         = regexp.MustCompile(`(?m)^go (\d+\.\d+)(?:\.\d+)?$`)
	dockerBuilderImagePattern  = regexp.MustCompile(`(?m)^FROM golang:(\d+\.\d+)(?:[^\s]*) AS builder$`)
)

func TestBuilderGoVersionMatchesGoMod(t *testing.T) {
	repoRoot := filepath.Join("..", "..")

	goMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatalf("read go.mod: %v", err)
	}

	dockerfile, err := os.ReadFile(filepath.Join(".", "Dockerfile"))
	if err != nil {
		t.Fatalf("read Dockerfile: %v", err)
	}

	goVersion := matchVersion(t, goDirectivePattern, string(goMod), "go directive")
	builderVersion := matchVersion(t, dockerBuilderImagePattern, string(dockerfile), "builder image")

	if builderVersion != goVersion {
		t.Fatalf("builder image Go version %q does not match go.mod version %q", builderVersion, goVersion)
	}
}

func matchVersion(t *testing.T, pattern *regexp.Regexp, input string, label string) string {
	t.Helper()

	matches := pattern.FindStringSubmatch(input)
	if len(matches) != 2 {
		t.Fatalf("could not find %s version", label)
	}

	return matches[1]
}
