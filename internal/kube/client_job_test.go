package kube

import (
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestNormalizeJobForApplyComparisonIgnoresControllerManagedFields(t *testing.T) {
	desired := &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "trainer",
			Labels:      map[string]string{"okdev.io/session": "sess1"},
			Annotations: map[string]string{"okdev.io/workload-api-version": "batch/v1"},
		},
		Spec: batchv1.JobSpec{
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{
						"okdev.io/session": "sess1",
					},
				},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{
						{Name: "trainer", Image: "ubuntu:22.04"},
					},
				},
			},
		},
	}

	existing := desired.DeepCopy()
	existing.Spec.Selector = &metav1.LabelSelector{
		MatchLabels: map[string]string{"batch.kubernetes.io/controller-uid": "abc123"},
	}
	existing.Spec.Template.Labels["batch.kubernetes.io/controller-uid"] = "abc123"
	existing.Spec.Template.Labels["batch.kubernetes.io/job-name"] = "trainer"
	existing.Spec.Template.Labels["controller-uid"] = "abc123"
	existing.Spec.Template.Labels["job-name"] = "trainer"

	normalizeJobForApplyComparison(desired)
	normalizeJobForApplyComparison(existing)

	if existing.Spec.Selector != nil {
		t.Fatalf("expected selector to be cleared, got %+v", existing.Spec.Selector)
	}
	if len(existing.Spec.Template.Labels) != len(desired.Spec.Template.Labels) {
		t.Fatalf("expected controller labels to be removed, got %+v vs %+v", existing.Spec.Template.Labels, desired.Spec.Template.Labels)
	}
}
