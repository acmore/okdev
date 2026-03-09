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
	if cfg.Spec.Sync.Syncthing.Image != DefaultSidecarImageForBinaryVersion(version.Version) {
		t.Fatalf("sync image default not set: %q", cfg.Spec.Sync.Syncthing.Image)
	}
	if !cfg.Spec.Sync.Syncthing.AutoInstallEnabled() {
		t.Fatal("expected syncthing autoinstall default true")
	}
	if cfg.Spec.SSH.User != "root" || cfg.Spec.SSH.RemotePort != 22 {
		t.Fatalf("ssh defaults not set: %+v", cfg.Spec.SSH)
	}
	if cfg.Spec.SSH.KeepAliveInterval != 10 || cfg.Spec.SSH.KeepAliveTimeout != 10 || cfg.Spec.SSH.KeepAliveCountMax != 30 {
		t.Fatalf("ssh keepalive defaults not set: %+v", cfg.Spec.SSH)
	}
	if cfg.Spec.SSH.AutoDetectPorts == nil || !*cfg.Spec.SSH.AutoDetectPorts {
		t.Fatal("expected ssh autoDetectPorts default true")
	}
	if cfg.Spec.Sidecar.Image == "" {
		t.Fatal("expected sidecar image default to be set")
	}
	if cfg.Spec.Sidecar.Image != DefaultSidecarImageForBinaryVersion(version.Version) {
		t.Fatalf("sidecar image default not set correctly: %q", cfg.Spec.Sidecar.Image)
	}
}

func TestSetDefaultsAutoDetectPortsFalse(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
		},
	}
	v := false
	cfg.Spec.SSH.AutoDetectPorts = &v
	cfg.SetDefaults()

	if cfg.Spec.SSH.AutoDetectPorts == nil || *cfg.Spec.SSH.AutoDetectPorts {
		t.Fatal("expected ssh autoDetectPorts to remain false when explicitly set")
	}
}

func TestSetDefaultsPersistentSessionNil(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	if cfg.Spec.SSH.PersistentSession != nil {
		t.Fatal("expected persistentSession to remain nil (off by default)")
	}
}

func TestSetDefaultsPersistentSessionExplicit(t *testing.T) {
	cfg := validConfig()
	v := true
	cfg.Spec.SSH.PersistentSession = &v
	cfg.SetDefaults()
	if cfg.Spec.SSH.PersistentSession == nil || !*cfg.Spec.SSH.PersistentSession {
		t.Fatal("expected persistentSession to remain true when explicitly set")
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

func TestValidateRejectsEmptySidecarImage(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	cfg.Spec.Sidecar.Image = ""
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for empty sidecar image")
	}
}

func TestValidateRejectsInvalidSSHKeepAlive(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	cfg.Spec.SSH.KeepAliveInterval = 20
	cfg.Spec.SSH.KeepAliveTimeout = 10
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid keepalive settings")
	}
}

func TestValidateAcceptsValidConfig(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestLifecycleSpecParsed(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Lifecycle.PostCreate = "make setup"
	cfg.Spec.Lifecycle.PreStop = "make clean"
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.Spec.Lifecycle.PostCreate != "make setup" {
		t.Fatalf("expected postCreate 'make setup', got %q", cfg.Spec.Lifecycle.PostCreate)
	}
	if cfg.Spec.Lifecycle.PreStop != "make clean" {
		t.Fatalf("expected preStop 'make clean', got %q", cfg.Spec.Lifecycle.PreStop)
	}
}

func TestLifecycleSpecEmpty(t *testing.T) {
	cfg := validConfig()
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
	if cfg.Spec.Lifecycle.PostCreate != "" || cfg.Spec.Lifecycle.PreStop != "" {
		t.Fatal("expected empty lifecycle spec by default")
	}
}

func TestDefaultSidecarImageForBinaryVersion(t *testing.T) {
	if got := DefaultSidecarImageForBinaryVersion("v0.2.1"); got != "ghcr.io/acmore/okdev:v0.2.1" {
		t.Fatalf("unexpected image for release version: %s", got)
	}
	if got := DefaultSidecarImageForBinaryVersion("0.0.0-dev"); got != "ghcr.io/acmore/okdev:edge" {
		t.Fatalf("unexpected image for dev version: %s", got)
	}
	if got := DefaultSidecarImageForBinaryVersion("unknown"); got != "ghcr.io/acmore/okdev:edge" {
		t.Fatalf("unexpected image for unknown version: %s", got)
	}
	if got := DefaultSidecarImageForBinaryVersion(""); got != "ghcr.io/acmore/okdev:edge" {
		t.Fatalf("unexpected image for empty version: %s", got)
	}
}
