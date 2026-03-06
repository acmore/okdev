package config

import (
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
)

// DevEnvironment is the top-level config structure for .okdev.yaml.
type DevEnvironment struct {
	APIVersion string     `yaml:"apiVersion"`
	Kind       string     `yaml:"kind"`
	Metadata   Metadata   `yaml:"metadata"`
	Spec       DevEnvSpec `yaml:"spec"`
}

type Metadata struct {
	Name string `yaml:"name"`
}

type DevEnvSpec struct {
	Namespace   string         `yaml:"namespace"`
	Session     SessionSpec    `yaml:"session"`
	Workspace   Workspace      `yaml:"workspace"`
	Sync        SyncSpec       `yaml:"sync"`
	Ports       []PortMapping  `yaml:"ports"`
	SSH         SSHSpec        `yaml:"ssh"`
	PodTemplate PodTemplateRef `yaml:"podTemplate"`
}

type SessionSpec struct {
	DefaultNameTemplate string `yaml:"defaultNameTemplate"`
	TTLHours            int    `yaml:"ttlHours"`
	IdleTimeoutMinutes  int    `yaml:"idleTimeoutMinutes"`
	Shareable           bool   `yaml:"shareable"`
	LockMode            string `yaml:"lockMode"`
}

type Workspace struct {
	MountPath string      `yaml:"mountPath"`
	PVC       PVCSettings `yaml:"pvc"`
}

type PVCSettings struct {
	ClaimName        string `yaml:"claimName"`
	Size             string `yaml:"size"`
	StorageClassName string `yaml:"storageClassName"`
}

type PodTemplateRef struct {
	Metadata MetadataMap    `yaml:"metadata"`
	Spec     corev1.PodSpec `yaml:"spec"`
}

type MetadataMap struct {
	Labels map[string]string `yaml:"labels"`
}

type SyncSpec struct {
	Paths     []string      `yaml:"paths"`
	Exclude   []string      `yaml:"exclude"`
	Engine    string        `yaml:"engine"`
	Syncthing SyncthingSpec `yaml:"syncthing"`
}

type SyncthingSpec struct {
	Version     string `yaml:"version"`
	AutoInstall *bool  `yaml:"autoInstall"`
	Image       string `yaml:"image"`
}

type PortMapping struct {
	Name   string `yaml:"name"`
	Local  int    `yaml:"local"`
	Remote int    `yaml:"remote"`
}

type SSHSpec struct {
	User           string `yaml:"user"`
	RemotePort     int    `yaml:"remotePort"`
	LocalPort      int    `yaml:"localPort"`
	PrivateKeyPath string `yaml:"privateKeyPath"`
}

func (d *DevEnvironment) SetDefaults() {
	if d == nil {
		return
	}
	if d.Spec.Namespace == "" {
		d.Spec.Namespace = "default"
	}
	if d.Spec.Sync.Engine == "" {
		d.Spec.Sync.Engine = "native"
	}
	if d.Spec.Sync.Syncthing.Version == "" {
		d.Spec.Sync.Syncthing.Version = "v1.29.7"
	}
	if d.Spec.Sync.Syncthing.AutoInstall == nil {
		v := true
		d.Spec.Sync.Syncthing.AutoInstall = &v
	}
	if d.Spec.Sync.Syncthing.Image == "" {
		d.Spec.Sync.Syncthing.Image = "ghcr.io/acmore/okdev-syncthing:v1.29.7"
	}
	if d.Spec.SSH.User == "" {
		d.Spec.SSH.User = "root"
	}
	if d.Spec.SSH.RemotePort == 0 {
		d.Spec.SSH.RemotePort = 22
	}
	if d.Spec.SSH.LocalPort == 0 {
		d.Spec.SSH.LocalPort = 2222
	}
	if d.Spec.Session.LockMode == "" {
		d.Spec.Session.LockMode = "none"
	}
}

func (d *DevEnvironment) Validate() error {
	if d == nil {
		return errors.New("config is nil")
	}
	if d.APIVersion == "" {
		return errors.New("apiVersion is required")
	}
	if d.APIVersion != "okdev.io/v1alpha1" {
		return fmt.Errorf("apiVersion must be okdev.io/v1alpha1, got %q", d.APIVersion)
	}
	if d.Kind == "" {
		return errors.New("kind is required")
	}
	if d.Kind != "DevEnvironment" {
		return fmt.Errorf("kind must be DevEnvironment, got %q", d.Kind)
	}
	if d.Metadata.Name == "" {
		return errors.New("metadata.name is required")
	}
	if d.Spec.Workspace.MountPath == "" {
		return errors.New("spec.workspace.mountPath is required")
	}
	if d.Spec.Sync.Engine != "native" && d.Spec.Sync.Engine != "syncthing" {
		return fmt.Errorf("spec.sync.engine must be native or syncthing, got %q", d.Spec.Sync.Engine)
	}
	if d.Spec.Session.LockMode != "none" && d.Spec.Session.LockMode != "advisory" && d.Spec.Session.LockMode != "exclusive" {
		return fmt.Errorf("spec.session.lockMode must be none|advisory|exclusive, got %q", d.Spec.Session.LockMode)
	}
	return nil
}

func (s SyncthingSpec) AutoInstallEnabled() bool {
	if s.AutoInstall == nil {
		return true
	}
	return *s.AutoInstall
}
