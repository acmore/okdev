package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
)

type fakePodLogsClient struct {
	mu           sync.Mutex
	containers   []string
	containerErr error
	streamErr    error
	streamCalls  []kube.LogStreamOptions
	streamByName map[string]string
}

func (f *fakePodLogsClient) PodContainerNames(_ context.Context, _, _ string) ([]string, error) {
	if f.containerErr != nil {
		return nil, f.containerErr
	}
	return append([]string(nil), f.containers...), nil
}

func (f *fakePodLogsClient) StreamPodLogs(_ context.Context, _, _ string, opts kube.LogStreamOptions, stdout io.Writer) error {
	f.mu.Lock()
	f.streamCalls = append(f.streamCalls, opts)
	f.mu.Unlock()
	if f.streamErr != nil {
		return f.streamErr
	}
	if data, ok := f.streamByName[opts.Container]; ok {
		_, err := io.WriteString(stdout, data)
		return err
	}
	return nil
}

func TestBuildLogsRequestValidation(t *testing.T) {
	if _, err := buildLogsRequest("dev", true, true, false, 0, false, 0); err == nil {
		t.Fatal("expected --all and --container conflict")
	}
	if _, err := buildLogsRequest("", false, true, false, -1, true, 0); err == nil {
		t.Fatal("expected negative tail to fail")
	}
	if _, err := buildLogsRequest("", false, true, false, 0, false, -time.Second); err == nil {
		t.Fatal("expected negative since to fail")
	}
}

func TestBuildLogsRequestTailDefaults(t *testing.T) {
	req, err := buildLogsRequest(" dev ", false, true, true, 25, true, 5*time.Minute)
	if err != nil {
		t.Fatalf("buildLogsRequest: %v", err)
	}
	if req.Container != "dev" || !req.Follow || !req.Previous {
		t.Fatalf("unexpected request: %+v", req)
	}
	if req.TailLines == nil || *req.TailLines != 25 {
		t.Fatalf("unexpected tail lines: %+v", req.TailLines)
	}
}

func TestRunLogsCommandDefaultsToTargetContainer(t *testing.T) {
	client := &fakePodLogsClient{}
	req := logsRequest{Follow: true}
	target := workload.TargetRef{PodName: "pod-1", Container: "dev"}

	if err := runLogsCommand(context.Background(), client, "default", target, req, io.Discard); err != nil {
		t.Fatalf("runLogsCommand: %v", err)
	}
	if len(client.streamCalls) != 1 {
		t.Fatalf("expected one stream call, got %d", len(client.streamCalls))
	}
	if got := client.streamCalls[0].Container; got != "dev" {
		t.Fatalf("expected target container dev, got %q", got)
	}
	if !client.streamCalls[0].Follow {
		t.Fatal("expected follow=true to be forwarded")
	}
}

func TestRunLogsCommandAllContainers(t *testing.T) {
	client := &fakePodLogsClient{
		containers: []string{"dev", "okdev-sidecar"},
		streamByName: map[string]string{
			"dev":           "app line\n",
			"okdev-sidecar": "sync line\n",
		},
	}
	var out bytes.Buffer

	err := runLogsCommand(context.Background(), client, "default", workload.TargetRef{PodName: "pod-1", Container: "dev"}, logsRequest{
		All:    true,
		Follow: true,
	}, &out)
	if err != nil {
		t.Fatalf("runLogsCommand: %v", err)
	}
	if len(client.streamCalls) != 2 {
		t.Fatalf("expected two stream calls, got %d", len(client.streamCalls))
	}
	got := out.String()
	for _, want := range []string{"[dev] app line", "[okdev-sidecar] sync line"} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected output to contain %q: %q", want, got)
		}
	}
}

func TestRunLogsCommandAllContainersRequiresContainers(t *testing.T) {
	client := &fakePodLogsClient{}
	err := runLogsCommand(context.Background(), client, "default", workload.TargetRef{PodName: "pod-1"}, logsRequest{All: true}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "has no containers") {
		t.Fatalf("expected no containers error, got %v", err)
	}
}

func TestStreamContainerLogsWithPrefixReturnsStreamError(t *testing.T) {
	client := &fakePodLogsClient{streamErr: errors.New("boom")}
	err := streamContainerLogsWithPrefix(context.Background(), client, "default", "pod-1", "dev", kube.LogStreamOptions{}, io.Discard)
	if err == nil || !strings.Contains(err.Error(), "boom") {
		t.Fatalf("expected stream error, got %v", err)
	}
}
