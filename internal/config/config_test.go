package config

import "testing"

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
	if cfg.Spec.Session.LockMode != "none" {
		t.Fatalf("lockMode default not set: %q", cfg.Spec.Session.LockMode)
	}
}

func TestValidateRejectsInvalidEngine(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
			Sync:      SyncSpec{Engine: "bad"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsInvalidLockMode(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Workspace: Workspace{MountPath: "/workspace"},
			Sync:      SyncSpec{Engine: "native"},
			Session:   SessionSpec{LockMode: "bad"},
		},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}
