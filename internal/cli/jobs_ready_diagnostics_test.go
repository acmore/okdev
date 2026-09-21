package cli

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
)

type canceledReadyOutputClient struct {
	*fakeJobsClient
	cancel context.CancelFunc
	output string
	err    error
}

func (c *canceledReadyOutputClient) StreamShInContainer(_ context.Context, _, _, _, _ string, stdout, _ io.Writer) error {
	_, err := io.WriteString(stdout, c.output)
	c.cancel()
	if err != nil {
		return err
	}
	return c.err
}

func TestJobsReadyRetainsCompletedFailureEvidenceAtCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, output, want string
		err                error
	}{
		{"oversized", "job" + strings.Repeat(" ", 4096), "expected job ID", nil},
		{"completed wrong identity", "wrong", "expected job ID", nil},
		{"partial identity", "wrong", "probe failed or timed out", context.Canceled},
		{"matching identity", "job", "probe failed or timed out", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			client := &canceledReadyOutputClient{fakeJobsClient: &fakeJobsClient{listOutputs: map[string][]string{"pod": {detachMetadataJSON("job", "pod", "dev", 100, "running", nil)}}}, cancel: cancel, output: tc.output, err: tc.err}
			_, err := runJobsReady(ctx, client, "ns", []kube.PodSummary{{Name: "pod"}}, "dev", "job", jobsReadyOptions{Probe: "health", ProbeTimeout: time.Minute})
			if !errors.Is(err, context.Canceled) || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("got %v; want cancellation and %q", err, tc.want)
			}
		})
	}
}
