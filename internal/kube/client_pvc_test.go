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
