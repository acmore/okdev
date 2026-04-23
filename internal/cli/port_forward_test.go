package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/kube"
	"github.com/acmore/okdev/internal/workload"
)

func TestParsePortForwardMappings(t *testing.T) {
	got, err := parsePortForwardMappings([]string{"8080:8080", "9000:9001"})
	if err != nil {
		t.Fatalf("parsePortForwardMappings: %v", err)
	}
	want := []string{"8080:8080", "9000:9001"}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected mappings: got=%v want=%v", got, want)
	}
}

func TestParsePortForwardMappingsRejectsInvalidValues(t *testing.T) {
	for _, tc := range [][]string{
		nil,
		{},
		{"8080"},
		{"abc:8080"},
		{"8080:def"},
		{"0:8080"},
		{"8080:0"},
		{"8080:8080:8080"},
	} {
		if _, err := parsePortForwardMappings(tc); err == nil {
			t.Fatalf("expected error for %v", tc)
		}
	}
}

func TestSelectSinglePortForwardPodByRoleRejectsAmbiguousMatch(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "master-0", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "master"}},
		{Name: "master-1", Phase: "Running", Labels: map[string]string{"okdev.io/workload-role": "master"}},
	}
	_, err := selectSinglePortForwardPod(pods, "", "master")
	if err == nil || !strings.Contains(err.Error(), "exactly one pod") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestSelectSinglePortForwardPodByPodName(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "worker-0", Phase: "Running"},
		{Name: "worker-1", Phase: "Running"},
	}
	got, err := selectSinglePortForwardPod(pods, "worker-1", "")
	if err != nil {
		t.Fatalf("selectSinglePortForwardPod: %v", err)
	}
	if got.Name != "worker-1" {
		t.Fatalf("expected worker-1, got %q", got.Name)
	}
}

type fakePortForwardClient struct {
	forwardedNamespace string
	forwardedPod       string
	forwardedMappings  []string
}

func (f *fakePortForwardClient) PortForward(ctx context.Context, namespace, pod string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	f.forwardedNamespace = namespace
	f.forwardedPod = pod
	f.forwardedMappings = append([]string(nil), forwards...)
	return context.Canceled
}

func TestRunPortForwardUsesDirectKubePortForward(t *testing.T) {
	client := &fakePortForwardClient{}
	target := workload.TargetRef{PodName: "okdev-sess-master-0", Container: "dev"}
	var out bytes.Buffer
	err := runPortForward(context.Background(), client, "demo", target, []string{"8080:8080"}, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context-canceled passthrough, got %v", err)
	}
	if client.forwardedNamespace != "demo" || client.forwardedPod != "okdev-sess-master-0" {
		t.Fatalf("unexpected forwarded target: ns=%q pod=%q", client.forwardedNamespace, client.forwardedPod)
	}
	wantMappings := []string{"8080:8080"}
	if !reflect.DeepEqual(wantMappings, client.forwardedMappings) {
		t.Fatalf("unexpected mappings: got=%v want=%v", client.forwardedMappings, wantMappings)
	}
	if !strings.Contains(out.String(), "Forwarding session target pod=okdev-sess-master-0") {
		t.Fatalf("expected startup message, got %q", out.String())
	}
}
