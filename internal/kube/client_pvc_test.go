package kube

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func TestPVCSpecDiffNoChange(t *testing.T) {
	base := pvcSpec("10Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, strPtr("fast"))
	mutableChanged, immutableChanged := pvcSpecDiff(base, base)
	if mutableChanged || immutableChanged {
		t.Fatalf("expected no changes, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func TestPVCSpecDiffMutableChange(t *testing.T) {
	existing := pvcSpec("10Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, strPtr("fast"))
	desired := pvcSpec("20Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, strPtr("fast"))
	mutableChanged, immutableChanged := pvcSpecDiff(existing, desired)
	if !mutableChanged || immutableChanged {
		t.Fatalf("expected mutable-only changes, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func TestPVCSpecDiffImmutableChange(t *testing.T) {
	existing := pvcSpec("10Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, strPtr("fast"))
	desired := pvcSpec("10Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, strPtr("fast"))
	mutableChanged, immutableChanged := pvcSpecDiff(existing, desired)
	if mutableChanged || !immutableChanged {
		t.Fatalf("expected immutable changes, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func TestPVCSpecDiffMutableAndImmutableChange(t *testing.T) {
	existing := pvcSpec("10Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, strPtr("fast"))
	desired := pvcSpec("20Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteMany}, strPtr("fast"))
	mutableChanged, immutableChanged := pvcSpecDiff(existing, desired)
	if !mutableChanged || !immutableChanged {
		t.Fatalf("expected both mutable and immutable changes, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func TestPVCSpecDiffIgnoresServerDefaultedFields(t *testing.T) {
	storageClass := "local-path"
	mode := corev1.PersistentVolumeFilesystem
	existing := pvcSpec("50Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, &storageClass)
	existing.VolumeName = "pvc-abc"
	existing.VolumeMode = &mode

	desired := pvcSpec("50Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, nil)
	mutableChanged, immutableChanged := pvcSpecDiff(existing, desired)
	if mutableChanged || immutableChanged {
		t.Fatalf("expected no drift for server-defaulted fields, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func TestPVCSpecDiffDetectsExplicitStorageClassChange(t *testing.T) {
	existingClass := "local-path"
	desiredClass := "fast-ssd"
	existing := pvcSpec("50Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, &existingClass)
	desired := pvcSpec("50Gi", []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}, &desiredClass)
	mutableChanged, immutableChanged := pvcSpecDiff(existing, desired)
	if mutableChanged || !immutableChanged {
		t.Fatalf("expected immutable storage class change, got mutable=%v immutable=%v", mutableChanged, immutableChanged)
	}
}

func pvcSpec(size string, accessModes []corev1.PersistentVolumeAccessMode, storageClass *string) corev1.PersistentVolumeClaimSpec {
	qty := resource.MustParse(size)
	return corev1.PersistentVolumeClaimSpec{
		AccessModes: accessModes,
		Resources: corev1.VolumeResourceRequirements{
			Requests: corev1.ResourceList{
				corev1.ResourceStorage: qty,
			},
		},
		StorageClassName: storageClass,
	}
}

func strPtr(v string) *string {
	return &v
}
