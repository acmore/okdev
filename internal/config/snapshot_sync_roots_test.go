package config

import "testing"

func TestWorkloadSnapshotTracksAdditionalSyncRoots(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.SetDefaults()
	cfg.Spec.Sync.Paths = []SyncPathSpec{{Local: ".", Remote: "/workspace"}}
	baseSnap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	base, _ := baseSnap.SHA256()

	// Adding a second mapping shapes the pod spec (auto-provisioned
	// volume), so the snapshot must change and trigger the reconcile
	// prompt.
	cfg.Spec.Sync.Paths = append(cfg.Spec.Sync.Paths, SyncPathSpec{Local: "../collected", Remote: "/data/results", Direction: "down"})
	multiSnap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	multi, _ := multiSnap.SHA256()
	if base == multi {
		t.Fatal("expected extra sync mapping to change the workload snapshot")
	}

	// Single-mapping sessions keep the legacy snapshot (omitempty), so
	// upgrading okdev does not force a reconcile.
	cfg.Spec.Sync.Paths = cfg.Spec.Sync.Paths[:1]
	againSnap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "", "")
	again, _ := againSnap.SHA256()
	if again != base {
		t.Fatal("single-mapping snapshot must stay on the legacy hash")
	}
}
