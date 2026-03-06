package config

import "testing"

func validConfig() *DevEnvironment {
	return &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
			Sync:      SyncSpec{Engine: "native"},
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
	if cfg.Spec.Sync.Engine != "native" {
		t.Fatalf("sync engine default not set: %q", cfg.Spec.Sync.Engine)
	}
	if !cfg.Spec.Sync.Syncthing.AutoInstallEnabled() {
		t.Fatal("expected syncthing autoinstall default true")
	}
	if cfg.Spec.SSH.User != "root" || cfg.Spec.SSH.RemotePort != 22 || cfg.Spec.SSH.LocalPort != 2222 {
		t.Fatalf("ssh defaults not set: %+v", cfg.Spec.SSH)
	}
}

func TestValidateRejectsInvalidEngine(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Engine = "bad"
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
	cfg.Spec.Sync.Engine = "syncthing"
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
