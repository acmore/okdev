package cli

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

// postCreate used to run only on the session's target pod. Every other pod in
// a multi-pod session never got it, and an in-place container restart on one
// of those pods left it permanently without its setup — postSync healed,
// postCreate did not. The e2e caught this only when target selection happened
// to land on a pod other than the one it restarted.
func TestRunPostCreateOnAllPodsFansOutLikePostSync(t *testing.T) {
	client := &fakePostSyncClient{
		pods: []kube.PodSummary{
			{Name: "master-0", Phase: "Running"},
			{Name: "worker-0", Phase: "Running", Annotations: map[string]string{annotationPostCreateDone: "true"}},
			{Name: "worker-1", Phase: "Running"},
			{Name: "worker-2", Phase: "Running", Deleting: true},
		},
	}

	var warnings bytes.Buffer
	summary, err := runPostCreateOnAllPods(
		context.Background(),
		client,
		"default",
		map[string]string{"okdev.io/managed": "true", "okdev.io/session": "sess"},
		"dev",
		"make setup",
		&warnings,
	)
	if err != nil {
		t.Fatalf("runPostCreateOnAllPods() error = %v", err)
	}
	if summary.Ran != 2 || summary.Skipped != 1 || summary.NotRunning != 1 {
		t.Fatalf("unexpected summary: %+v", summary)
	}
	client.mu.Lock()
	defer client.mu.Unlock()
	if len(client.execCalls) != 2 {
		t.Fatalf("expected 2 exec calls, got %#v", client.execCalls)
	}
}

// The regression the e2e hit: a pod that is not the target, already marked
// done, whose container restarted after the marker was written. It must replay.
func TestRunPostCreateReplaysOnARestartedNonTargetPod(t *testing.T) {
	markedAt := time.Now().Add(-10 * time.Minute)
	restartedAt := time.Now().Add(-1 * time.Minute)

	client := &fakePostSyncClient{
		pods: []kube.PodSummary{
			{
				Name:  "master-0",
				Phase: "Running",
				Annotations: map[string]string{
					annotationPostCreateDone:  "true",
					annotationPostCreateState: hookStateDone,
					annotationPostCreateAt:    markedAt.Format(time.RFC3339),
				},
				ContainerStarts: []kube.ContainerStart{{Name: "dev", StartedAt: restartedAt}},
			},
		},
	}

	var warnings bytes.Buffer
	summary, err := runPostCreateOnAllPods(
		context.Background(),
		client,
		"default",
		map[string]string{"okdev.io/session": "sess"},
		"dev",
		"make setup",
		&warnings,
	)
	if err != nil {
		t.Fatalf("runPostCreateOnAllPods() error = %v", err)
	}
	if summary.Ran != 1 {
		t.Fatalf("a restarted container must replay postCreate, got %+v", summary)
	}
	if got := warnings.String(); got == "" {
		t.Fatal("re-running after an in-place restart must say so")
	}
}

// A pod marked done whose container has not restarted must stay skipped, so
// the fanout does not re-run setup on every up.
func TestRunPostCreateSkipsAFreshlyDonePod(t *testing.T) {
	markedAt := time.Now().Add(-1 * time.Minute)
	startedAt := time.Now().Add(-10 * time.Minute)

	client := &fakePostSyncClient{
		pods: []kube.PodSummary{
			{
				Name:  "master-0",
				Phase: "Running",
				Annotations: map[string]string{
					annotationPostCreateDone:  "true",
					annotationPostCreateState: hookStateDone,
					annotationPostCreateAt:    markedAt.Format(time.RFC3339),
				},
				ContainerStarts: []kube.ContainerStart{{Name: "dev", StartedAt: startedAt}},
			},
		},
	}

	var warnings bytes.Buffer
	summary, err := runPostCreateOnAllPods(
		context.Background(),
		client,
		"default",
		map[string]string{"okdev.io/session": "sess"},
		"dev",
		"make setup",
		&warnings,
	)
	if err != nil {
		t.Fatalf("runPostCreateOnAllPods() error = %v", err)
	}
	if summary.Ran != 0 || summary.Skipped != 1 {
		t.Fatalf("a fresh done marker must be skipped, got %+v", summary)
	}
}
