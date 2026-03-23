package cli

import (
	"context"
	"errors"
	"io"
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

func (f *fakeDevShellExecutor) StreamShInContainer(_ context.Context, _, _ string, container, script string, stdout, stderr io.Writer) error {
	f.container = container
	f.lastScript = script
	idx := f.calls
	f.calls++
	if idx < len(f.outputs) && stdout != nil {
		_, _ = stdout.Write(f.outputs[idx])
	}
	if idx < len(f.errors) {
		return f.errors[idx]
	}
	return nil
}

func TestDetectDevTmux(t *testing.T) {
	tests := []struct {
		name          string
		fake          *fakeDevShellExecutor
		wantStatus    string
		wantInstaller string
		wantErr       string
	}{
		{name: "present", fake: newFakeDevShellExecutor([]string{"present:none"}, nil), wantStatus: "present", wantInstaller: "none"},
		{name: "install apt-get", fake: newFakeDevShellExecutor([]string{"install:apt-get"}, nil), wantStatus: "install", wantInstaller: "apt-get"},
		{name: "no-root", fake: newFakeDevShellExecutor([]string{"no-root:none"}, nil), wantStatus: "no-root", wantInstaller: "none"},
		{name: "unavailable", fake: newFakeDevShellExecutor([]string{"unavailable:none"}, nil), wantStatus: "unavailable", wantInstaller: "none"},
		{name: "exec error", fake: newFakeDevShellExecutor(nil, []error{errors.New("boom")}), wantErr: "boom"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			status, installer, err := detectDevTmux(context.Background(), tt.fake, "default", "okdev-test")
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if status != tt.wantStatus {
				t.Fatalf("expected status %q, got %q", tt.wantStatus, status)
			}
			if installer != tt.wantInstaller {
				t.Fatalf("expected installer %q, got %q", tt.wantInstaller, installer)
			}
			if tt.fake.container != "dev" {
				t.Fatalf("expected dev container, got %q", tt.fake.container)
			}
		})
	}
}

func TestInstallDevTmux(t *testing.T) {
	tests := []struct {
		name          string
		fake          *fakeDevShellExecutor
		installer     string
		wantInstalled bool
		wantErr       string
	}{
		{name: "installed", fake: newFakeDevShellExecutor([]string{"__OKDEV_TMUX_STATUS__=installed:apt-get"}, nil), installer: "apt-get", wantInstalled: true},
		{name: "installed via fallback detect", fake: newFakeDevShellExecutor([]string{"", "present:none"}, nil), installer: "apt-get"},
		{name: "no-root", fake: newFakeDevShellExecutor([]string{"__OKDEV_TMUX_STATUS__=no-root:none"}, nil), installer: "apt-get", wantErr: "not running as root"},
		{name: "unavailable", fake: newFakeDevShellExecutor([]string{"__OKDEV_TMUX_STATUS__=unavailable:apk"}, nil), installer: "apk", wantErr: devTmuxLogPath},
		{name: "exec error", fake: newFakeDevShellExecutor(nil, []error{errors.New("boom")}), installer: "apt-get", wantErr: "boom"},
		{name: "empty output fallback error", fake: newFakeDevShellExecutor([]string{"", ""}, []error{nil, errors.New("detect boom")}), installer: "apt-get", wantErr: devTmuxLogPath},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotInstalled, _, err := installDevTmux(context.Background(), tt.fake, "default", "okdev-test", tt.installer, nil)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotInstalled != tt.wantInstalled {
				t.Fatalf("expected installed=%v, got %v", tt.wantInstalled, gotInstalled)
			}
			if tt.fake.container != "dev" {
				t.Fatalf("expected dev container, got %q", tt.fake.container)
			}
		})
	}
}

func TestInstallDevTmuxDetails(t *testing.T) {
	tests := []struct {
		name       string
		fake       *fakeDevShellExecutor
		installer  string
		wantDetail string
	}{
		{name: "installed with installer", fake: newFakeDevShellExecutor([]string{"__OKDEV_TMUX_STATUS__=installed:apt-get"}, nil), installer: "apt-get", wantDetail: "installed in dev container via apt-get"},
		{name: "installed no installer", fake: newFakeDevShellExecutor([]string{"__OKDEV_TMUX_STATUS__=installed:none"}, nil), installer: "none", wantDetail: "installed in dev container"},
		{name: "present via fallback detect", fake: newFakeDevShellExecutor([]string{"", "present:none"}, nil), installer: "apt-get", wantDetail: "ready in dev container"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, gotDetail, err := installDevTmux(context.Background(), tt.fake, "default", "okdev-test", tt.installer, nil)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotDetail != tt.wantDetail {
				t.Fatalf("expected detail %q, got %q", tt.wantDetail, gotDetail)
			}
		})
	}
}

func TestInterpretTmuxStatus(t *testing.T) {
	tests := []struct {
		name          string
		status        string
		installer     string
		wantInstalled bool
		wantDetail    string
		wantErr       string
	}{
		{name: "present", status: "present", installer: "none", wantDetail: "ready in dev container"},
		{name: "installed with installer", status: "installed", installer: "apt-get", wantInstalled: true, wantDetail: "installed in dev container via apt-get"},
		{name: "installed no installer", status: "installed", installer: "none", wantInstalled: true, wantDetail: "installed in dev container"},
		{name: "no-root", status: "no-root", installer: "none", wantErr: "not running as root"},
		{name: "unavailable with installer", status: "unavailable", installer: "apk", wantErr: devTmuxLogPath},
		{name: "unavailable no pkg mgr", status: "unavailable", installer: "none", wantErr: "no supported package manager found"},
		{name: "unexpected", status: "mystery", installer: "none", wantErr: "unexpected tmux prepare result"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotInstalled, gotDetail, err := interpretTmuxStatus(tt.status, tt.installer, tt.status+":"+tt.installer)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("expected error containing %q, got %v", tt.wantErr, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if gotInstalled != tt.wantInstalled {
				t.Fatalf("expected installed=%v, got %v", tt.wantInstalled, gotInstalled)
			}
			if gotDetail != tt.wantDetail {
				t.Fatalf("expected detail %q, got %q", tt.wantDetail, gotDetail)
			}
		})
	}
}

func TestDevTmuxDetailIfReady(t *testing.T) {
	t.Run("ready", func(t *testing.T) {
		fake := newFakeDevShellExecutor([]string{"present:none"}, nil)
		detail, ok := devTmuxDetailIfReady(context.Background(), fake, "default", "okdev-test")
		if !ok {
			t.Fatal("expected ready detail")
		}
		if detail != "ready in dev container" {
			t.Fatalf("unexpected detail %q", detail)
		}
	})

	t.Run("not ready", func(t *testing.T) {
		fake := newFakeDevShellExecutor([]string{"unavailable:none"}, nil)
		if detail, ok := devTmuxDetailIfReady(context.Background(), fake, "default", "okdev-test"); ok || detail != "" {
			t.Fatalf("expected no ready detail, got %q ok=%v", detail, ok)
		}
	})
}

func TestDevTmuxScriptsIncludeKnownPackageManagers(t *testing.T) {
	for _, want := range []string{
		"echo install:apk",
		"echo install:apt-get",
		"apk add --no-cache tmux",
		"apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends tmux",
		"dnf install -y tmux",
		"microdnf install -y tmux",
		"yum install -y tmux",
		"/var/okdev/.tmux-install-attempted",
		devTmuxLogPath,
	} {
		if !strings.Contains(devTmuxDetectScript+"\n"+devTmuxInstallScript, want) {
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
