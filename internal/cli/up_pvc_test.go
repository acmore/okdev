package cli

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

type fakePVCReconciler struct {
	existing map[string]bool
	applied  []string
}

func (f *fakePVCReconciler) PersistentVolumeClaimExists(_ context.Context, _ string, name string) (bool, error) {
	return f.existing[name], nil
}

func (f *fakePVCReconciler) Apply(_ context.Context, _ string, manifest []byte) error {
	f.applied = append(f.applied, string(manifest))
	return nil
}

func TestReconcileMissingPVCsCreatesOnlyMissingClaims(t *testing.T) {
	k := &fakePVCReconciler{
		existing: map[string]bool{
			"team-workspace": true,
			"team-datasets":  false,
		},
	}
	volumes := []corev1.Volume{
		{
			Name: "workspace",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-workspace"},
			},
		},
		{
			Name: "datasets",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-datasets"},
			},
		},
		{
			Name: "datasets-dup",
			VolumeSource: corev1.VolumeSource{
				PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: "team-datasets"},
			},
		},
	}

	created, err := reconcileMissingPVCs(context.Background(), k, "dev", volumes, "20Gi", "fast", map[string]string{"okdev.io/managed": "true"}, nil)
	if err != nil {
		t.Fatalf("reconcileMissingPVCs error: %v", err)
	}
	if len(created) != 1 || created[0] != "team-datasets" {
		t.Fatalf("unexpected created pvc list: %#v", created)
	}
	if len(k.applied) != 1 {
		t.Fatalf("expected 1 applied pvc manifest, got %d", len(k.applied))
	}
}
