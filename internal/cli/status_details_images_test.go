package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
)

func TestRenderImagesLine(t *testing.T) {
	line := renderImagesLine(buildDetailedImages([]kube.ContainerImage{
		{Name: "dev", Image: "ghcr.io/org/dev:cuda12", Digest: "sha256:1a2b3c4d5e6f7a8b9c0d"},
		{Name: "okdev-sidecar", Image: "okdev-sidecar:v0.8.0"},
	}))
	// The digest is truncated on purpose: it is here to be compared between
	// pods, not to be resolved.
	if line != "images: dev=ghcr.io/org/dev:cuda12 (sha256:1a2b3c4d5e6f), okdev-sidecar=okdev-sidecar:v0.8.0" {
		t.Fatalf("unexpected line %q", line)
	}
	if renderImagesLine(nil) != "" {
		t.Fatal("no images must render no line, not an empty label")
	}
}

func TestBuildDetailedImagesSkipsEmptyImages(t *testing.T) {
	images := buildDetailedImages([]kube.ContainerImage{
		{Name: "dev", Image: "ubuntu:22.04"},
		{Name: "ghost", Image: "   "},
	})
	if len(images) != 1 || images[0].Container != "dev" {
		t.Fatalf("unexpected images %+v", images)
	}
}

func TestShortImageDigestLeavesNonSHAValuesAlone(t *testing.T) {
	if got := shortImageDigest("sha256:0123456789abcdef"); got != "sha256:0123456789ab" {
		t.Fatalf("got %q", got)
	}
	if got := shortImageDigest("weird-id"); got != "weird-id" {
		t.Fatalf("a non-sha id must pass through verbatim, got %q", got)
	}
	if got := shortImageDigest(""); got != "" {
		t.Fatalf("got %q", got)
	}
}

func TestDetailedStatusImagesRenderUnderPod(t *testing.T) {
	var sb strings.Builder
	printDetailedStatus(&sb, detailedStatus{
		Session: "sess",
		Pods: []detailedStatusPod{{
			Name:   "okdev-sess-abc-master-0",
			Phase:  "Running",
			Ready:  "2/2",
			Images: []detailedStatusImage{{Container: "dev", Image: "ubuntu:22.04", Digest: "sha256:aaaabbbbccccdddd"}},
		}},
	})
	got := sb.String()
	if !strings.Contains(got, "images: dev=ubuntu:22.04 (sha256:aaaabbbbcccc)") {
		t.Fatalf("expected the image line under the pod, got:\n%s", got)
	}
}
