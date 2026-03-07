package config

import (
	"testing"

	"github.com/acmore/okdev/internal/version"
)

func validConfig() *DevEnvironment {
	return &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
			Sync:      SyncSpec{Engine: "syncthing"},
			Session:   SessionSpec{},
		},
	}
}

func TestSetDefaults(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
		},
	}
	cfg.SetDefaults()

	if cfg.Spec.Namespace != "default" {
		t.Fatalf("namespace default not set: %q", cfg.Spec.Namespace)
	}
	if cfg.Spec.Sync.Engine != "syncthing" {
		t.Fatalf("sync engine default not set: %q", cfg.Spec.Sync.Engine)
	}
	if cfg.Spec.Sync.Syncthing.Image != DefaultSyncthingImageForBinaryVersion(version.Version) {
		t.Fatalf("sync image default not set: %q", cfg.Spec.Sync.Syncthing.Image)
	}
	if !cfg.Spec.Sync.Syncthing.AutoInstallEnabled() {
		t.Fatal("expected syncthing autoinstall default true")
	}
	if cfg.Spec.SSH.User != "root" || cfg.Spec.SSH.RemotePort != 22 || cfg.Spec.SSH.LocalPort != 2222 {
		t.Fatalf("ssh defaults not set: %+v", cfg.Spec.SSH)
	}
	if cfg.Spec.SSH.Mode != DefaultSSHMode {
		t.Fatalf("ssh mode default not set: %q", cfg.Spec.SSH.Mode)
	}
}

func TestValidateRejectsInvalidEngine(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Engine = "native"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsNegativeTTL(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Session.TTLHours = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsInvalidSyncPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []string{"./local-only"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsSyncthingMultiplePaths(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []string{"./a:/workspace/a", "./b:/workspace/b"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsDuplicateLocalPorts(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Ports = []PortMapping{
		{Name: "a", Local: 8080, Remote: 8080},
		{Name: "b", Local: 8080, Remote: 18080},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsInvalidSSHPort(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.LocalPort = 70000
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsInvalidSSHMode(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.Mode = "invalid"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsEmptySSHSidecarImage(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.Mode = "sidecar"
	cfg.Spec.SSH.SidecarImage = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestDefaultSyncthingImageForBinaryVersion(t *testing.T) {
	if got := DefaultSyncthingImageForBinaryVersion("v0.2.1"); got != "ghcr.io/acmore/okdev:v0.2.1" {
		t.Fatalf("unexpected image for release version: %s", got)
	}
	if got := DefaultSyncthingImageForBinaryVersion("0.0.0-dev"); got != "ghcr.io/acmore/okdev:edge" {
		t.Fatalf("unexpected image for dev version: %s", got)
	}
}

func TestDefaultSSHSidecarImageForBinaryVersion(t *testing.T) {
	if got := DefaultSSHSidecarImageForBinaryVersion("v0.2.1"); got != "ghcr.io/acmore/okdev-sshd:v0.2.1" {
		t.Fatalf("unexpected image for release version: %s", got)
	}
	if got := DefaultSSHSidecarImageForBinaryVersion("unknown"); got != "ghcr.io/acmore/okdev-sshd:edge" {
		t.Fatalf("unexpected image for unknown version: %s", got)
	}
	if got := DefaultSSHSidecarImageForBinaryVersion("dev-build"); got != "ghcr.io/acmore/okdev-sshd:edge" {
		t.Fatalf("unexpected image for dev version: %s", got)
	}
}
