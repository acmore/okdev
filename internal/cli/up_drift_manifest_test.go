package cli

import (
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
)

// The manifest is where a workload's content lives, so "the manifest changed"
// without saying how is barely more useful than silence — which is what a bare
// digest in the snapshot amounted to.
func TestDriftShowsWhatChangedInTheManifest(t *testing.T) {
	before := &config.LastAppliedWorkloadSpec{
		Version:      config.SnapshotVersion,
		WorkloadKind: "pod",
		ManifestPath: "pod.yaml",
		Manifest: `apiVersion: v1
kind: Pod
spec:
  containers:
    - name: dev
      image: alpine:3.19
`,
	}
	after := *before
	after.Manifest = strings.Replace(before.Manifest, "alpine:3.19", "alpine:3.20", 1)

	oldJSON, err := before.JSON()
	if err != nil {
		t.Fatal(err)
	}
	oldHash, err := before.SHA256()
	if err != nil {
		t.Fatal(err)
	}

	got := detectDrift(&after, oldJSON, oldHash)
	if got.Kind != driftChanged {
		t.Fatalf("expected drift, got %v", got.Kind)
	}
	if !strings.Contains(got.ManifestDiff, "- ") || !strings.Contains(got.ManifestDiff, "alpine:3.19") {
		t.Fatalf("the diff must show the line that changed:\n%s", got.ManifestDiff)
	}
	if !strings.Contains(got.ManifestDiff, "alpine:3.20") {
		t.Fatalf("the diff must show what it changed to:\n%s", got.ManifestDiff)
	}
	if got.ManifestPath != "pod.yaml" {
		t.Fatalf("the diff must name the file: %q", got.ManifestPath)
	}
	// The manifest must not also appear inside the spec diff, where it would
	// render as one enormous changed line and bury everything else.
	if strings.Contains(got.Diff, "alpine") {
		t.Fatalf("the manifest belongs only in its own diff:\n%s", got.Diff)
	}
}
