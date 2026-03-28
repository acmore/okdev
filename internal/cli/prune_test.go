package cli

import (
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

func TestSessionLastActiveUsesAnnotationWhenValid(t *testing.T) {
	createdAt := time.Unix(10, 0)
	annotated := createdAt.Add(5 * time.Minute).UTC()
	pod := kube.PodSummary{
		CreatedAt: createdAt,
		Annotations: map[string]string{
			"okdev.io/last-attach": annotated.Format(time.RFC3339),
		},
	}
	got, warned := sessionLastActive(pod)
	if warned {
		t.Fatal("did not expect warning for valid timestamp")
	}
	if !got.Equal(annotated) {
		t.Fatalf("expected annotated time %s, got %s", annotated, got)
	}
}

func TestSessionLastActiveFallsBackWhenAnnotationInvalid(t *testing.T) {
	createdAt := time.Unix(10, 0)
	pod := kube.PodSummary{
		CreatedAt: createdAt,
		Annotations: map[string]string{
			"okdev.io/last-attach": "not-a-time",
		},
	}
	got, warned := sessionLastActive(pod)
	if !warned {
		t.Fatal("expected warning for invalid timestamp")
	}
	if !got.Equal(createdAt) {
		t.Fatalf("expected createdAt fallback %s, got %s", createdAt, got)
	}
}
