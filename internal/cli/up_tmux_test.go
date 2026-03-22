package cli

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type fakeDevShellExecutor struct {
	outputs    [][]byte
	errors     []error
	container  string
	lastScript string
	calls      int
}

func (f *fakeDevShellExecutor) ExecShInContainer(_ context.Context, _, _ string, container, script string) ([]byte, error) {
	f.container = container
	f.lastScript = script
	idx := f.calls
	f.calls++
	var out []byte
	var err error
	if idx < len(f.outputs) {
		out = f.outputs[idx]
	}
	if idx < len(f.errors) {
		err = f.errors[idx]
	}
	return out, err
}

func TestEnsureEmbeddedTmux(t *testing.T) {
	tests := []struct {
		name          string
		fake          *fakeDevShellExecutor
		wantInstalled bool
		wantErr       string
		wantCalls     int
	}{
		{name: "present", fake: newFakeDevShellExecutor([]string{"present:none"}, nil), wantCalls: 1},
		{name: "installed", fake: newFakeDevShellExecutor([]string{"install:apt-get", "installed:apt-get"}, nil), wantInstalled: true, wantCalls: 2},
		{name: "no-root detect", fake: newFakeDevShellExecutor([]string{"no-root:none"}, nil), wantErr: "not running as root", wantCalls: 1},
		{name: "no-root install", fake: newFakeDevShellExecutor([]string{"install:apt-get", "no-root:none"}, nil), wantErr: "not running as root", wantCalls: 2},
		{name: "unavailable", fake: newFakeDevShellExecutor([]string{"install:apk", "unavailable:apk"}, nil), wantErr: "unavailable after best-effort", wantCalls: 2},
		{name: "unexpected", fake: newFakeDevShellExecutor([]string{"mystery"}, nil), wantErr: "unexpected tmux prepare result", wantCalls: 1},
		{name: "exec error", fake: newFakeDevShellExecutor(nil, []error{errors.New("boom")}), wantErr: "boom", wantCalls: 1},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotInstalled, _, err := ensureEmbeddedTmux(context.Background(), tt.fake, "default", "okdev-test", nil)
			if tt.wantErr == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
			}
			if gotInstalled != tt.wantInstalled {
				t.Fatalf("expected installed=%v, got %v", tt.wantInstalled, gotInstalled)
			}
			if tt.fake.container != "dev" {
				t.Fatalf("expected dev container, got %q", tt.fake.container)
			}
			if tt.fake.calls != tt.wantCalls {
				t.Fatalf("expected %d exec calls, got %d", tt.wantCalls, tt.fake.calls)
			}
		})
	}
}

func TestEnsureEmbeddedTmuxDetails(t *testing.T) {
	tests := []struct {
		name       string
		fake       *fakeDevShellExecutor
		wantDetail string
	}{
		{name: "present", fake: newFakeDevShellExecutor([]string{"present:none"}, nil), wantDetail: "ready in dev container"},
		{name: "installed with installer", fake: newFakeDevShellExecutor([]string{"install:apt-get", "installed:apt-get"}, nil), wantDetail: "installed in dev container via apt-get"},
		{name: "installed no installer", fake: newFakeDevShellExecutor([]string{"install:none", "installed:none"}, nil), wantDetail: "installed in dev container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotDetail, err := ensureEmbeddedTmux(context.Background(), tt.fake, "default", "okdev-test", nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotDetail != tt.wantDetail {
				t.Fatalf("expected detail %q, got %q", tt.wantDetail, gotDetail)
			}
		})
	}
}

func TestEnsureEmbeddedTmuxProgress(t *testing.T) {
	fake := newFakeDevShellExecutor([]string{"install:apt-get", "installed:apt-get"}, nil)
	var phases []string
	_, _, err := ensureEmbeddedTmux(context.Background(), fake, "default", "okdev-test", func(phase string) {
		phases = append(phases, phase)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(phases) != 1 || phases[0] != "installing via apt-get" {
		t.Fatalf("unexpected phases: %v", phases)
	}
}

func TestEmbeddedTmuxScriptsIncludeKnownPackageManagers(t *testing.T) {
	for _, want := range []string{
		"echo install:apk",
		"echo install:apt-get",
		"apk add --no-cache tmux",
		"apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends tmux",
		"dnf install -y tmux",
		"microdnf install -y tmux",
		"yum install -y tmux",
		"/var/okdev/.tmux-install-attempted",
	} {
		if !strings.Contains(embeddedTmuxDetectScript+"\n"+embeddedTmuxInstallScript, want) {
			t.Fatalf("expected scripts to contain %q", want)
		}
	}
}

func newFakeDevShellExecutor(outputs []string, errs []error) *fakeDevShellExecutor {
	fake := &fakeDevShellExecutor{}
	for _, item := range outputs {
		fake.outputs = append(fake.outputs, []byte(item+"\n"))
	}
	fake.errors = errs
	return fake
}
