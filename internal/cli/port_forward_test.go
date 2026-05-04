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

func TestSelectSinglePortForwardPodRejectsAmbiguousMatch(t *testing.T) {
	pods := []kube.PodSummary{
		{Name: "master-0", Phase: "Running"},
		{Name: "master-1", Phase: "Running"},
	}
	_, err := selectSinglePortForwardPod(pods)
	if err == nil || !strings.Contains(err.Error(), "exactly one pod") {
		t.Fatalf("expected ambiguity error, got %v", err)
	}
}

func TestSelectSinglePortForwardPodRejectsEmpty(t *testing.T) {
	_, err := selectSinglePortForwardPod(nil)
	if err == nil || !strings.Contains(err.Error(), "exactly one pod") {
		t.Fatalf("expected empty error, got %v", err)
	}
}

func TestSelectSinglePortForwardPodReturnsSingle(t *testing.T) {
	pods := []kube.PodSummary{{Name: "worker-0", Phase: "Running"}}
	got, err := selectSinglePortForwardPod(pods)
	if err != nil {
		t.Fatalf("selectSinglePortForwardPod: %v", err)
	}
	if got.Name != "worker-0" {
		t.Fatalf("expected worker-0, got %q", got.Name)
	}
}

func TestSplitPortForwardArgs(t *testing.T) {
	for _, tc := range []struct {
		name         string
		args         []string
		wantSession  []string
		wantMappings []string
	}{
		{"empty", nil, nil, nil},
		{"mapping only", []string{"8080:8080"}, nil, []string{"8080:8080"}},
		{"session and mapping", []string{"my-sess", "8080:8080"}, []string{"my-sess"}, []string{"8080:8080"}},
		{"single non-mapping stays in mappings", []string{"8080"}, nil, []string{"8080"}},
		{"two mappings", []string{"8080:8080", "9000:9000"}, nil, []string{"8080:8080", "9000:9000"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			gotSession, gotMappings := splitPortForwardArgs(tc.args)
			if !reflect.DeepEqual(tc.wantSession, gotSession) {
				t.Fatalf("session: got=%v want=%v", gotSession, tc.wantSession)
			}
			if !reflect.DeepEqual(tc.wantMappings, gotMappings) {
				t.Fatalf("mappings: got=%v want=%v", gotMappings, tc.wantMappings)
			}
		})
	}
}

type fakePortForwardClient struct {
	forwardedNamespace string
	forwardedPod       string
	forwardedAddresses []string
	forwardedMappings  []string
}

func (f *fakePortForwardClient) PortForwardOnAddresses(ctx context.Context, namespace, pod string, addresses []string, forwards []string, stdout io.Writer, stderr io.Writer) error {
	f.forwardedNamespace = namespace
	f.forwardedPod = pod
	f.forwardedAddresses = append([]string(nil), addresses...)
	f.forwardedMappings = append([]string(nil), forwards...)
	return context.Canceled
}

func TestRunPortForwardUsesDirectKubePortForward(t *testing.T) {
	client := &fakePortForwardClient{}
	target := workload.TargetRef{PodName: "okdev-sess-master-0"}
	var out bytes.Buffer
	err := runPortForward(context.Background(), client, "demo", target, []string{"localhost", "0.0.0.0"}, []string{"8080:8080"}, &out)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("expected context-canceled passthrough, got %v", err)
	}
	if client.forwardedNamespace != "demo" || client.forwardedPod != "okdev-sess-master-0" {
		t.Fatalf("unexpected forwarded target: ns=%q pod=%q", client.forwardedNamespace, client.forwardedPod)
	}
	wantAddresses := []string{"localhost", "0.0.0.0"}
	if !reflect.DeepEqual(wantAddresses, client.forwardedAddresses) {
		t.Fatalf("unexpected addresses: got=%v want=%v", client.forwardedAddresses, wantAddresses)
	}
	wantMappings := []string{"8080:8080"}
	if !reflect.DeepEqual(wantMappings, client.forwardedMappings) {
		t.Fatalf("unexpected mappings: got=%v want=%v", client.forwardedMappings, wantMappings)
	}
	if !strings.Contains(out.String(), "Forwarding to pod=okdev-sess-master-0") {
		t.Fatalf("expected startup message, got %q", out.String())
	}
}

func TestNormalizePortForwardAddresses(t *testing.T) {
	for _, tc := range []struct {
		name    string
		input   []string
		want    []string
		wantErr string
	}{
		{name: "default localhost", input: nil, want: []string{"localhost"}},
		{name: "single explicit", input: []string{"0.0.0.0"}, want: []string{"0.0.0.0"}},
		{name: "comma separated", input: []string{"localhost,0.0.0.0"}, want: []string{"localhost", "0.0.0.0"}},
		{name: "repeated and deduped", input: []string{"localhost", "0.0.0.0", "localhost"}, want: []string{"localhost", "0.0.0.0"}},
		{name: "trim spaces", input: []string{" localhost ", " 0.0.0.0 "}, want: []string{"localhost", "0.0.0.0"}},
		{name: "reject empty", input: []string{"localhost,"}, wantErr: "invalid --address value"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := normalizePortForwardAddresses(tc.input)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tc.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("normalizePortForwardAddresses: %v", err)
			}
			if !reflect.DeepEqual(tc.want, got) {
				t.Fatalf("unexpected addresses: got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestPortForwardCommandRejectsPodAndRoleTogether(t *testing.T) {
	cmd := newPortForwardCmd(&Options{})
	cmd.SetArgs([]string{"--pod", "a", "--role", "worker", "8080:8080"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "mutually exclusive") {
		t.Fatalf("expected mutual exclusion error, got %v", err)
	}
}

func TestPortForwardCommandRejectsMissingMappings(t *testing.T) {
	cmd := newPortForwardCmd(&Options{})
	cmd.SetArgs(nil)
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "requires at least one") {
		t.Fatalf("expected missing mapping error, got %v", err)
	}
}

func TestPortForwardCommandExposesAddressFlag(t *testing.T) {
	cmd := newPortForwardCmd(&Options{})
	flag := cmd.Flags().Lookup("address")
	if flag == nil {
		t.Fatal("expected --address flag")
	}
	got, err := cmd.Flags().GetStringSlice("address")
	if err != nil {
		t.Fatalf("GetStringSlice(address): %v", err)
	}
	want := []string{"localhost"}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected default addresses: got=%v want=%v", got, want)
	}
}
