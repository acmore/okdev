package cli

import (
	"testing"

	"github.com/acmore/okdev/internal/config"
)

func TestDiscoveryLabelsForSessionSkipsEmptyWorkloadType(t *testing.T) {
	cfg := &config.DevEnvironment{}
	cfg.Metadata.Name = "trainer"

	labels := discoveryLabelsForSession(cfg, "sess1")
	if _, ok := labels["okdev.io/workload-type"]; ok {
		t.Fatalf("expected empty workload type to be omitted, got %+v", labels)
	}
}
