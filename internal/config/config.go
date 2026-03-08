package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/version"
	corev1 "k8s.io/api/core/v1"
)

const (
	DefaultSyncthingVersion       = "v1.29.7"
	DefaultSidecarImageRepository = "ghcr.io/acmore/okdev"
	DefaultSidecarImageFallback   = "edge"
	DefaultWorkspacePVCSize       = "50Gi"
)

var DefaultSidecarImage = DefaultSidecarImageForBinaryVersion(version.Version)

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
	KubeContext string         `yaml:"kubeContext"`
	Session     SessionSpec    `yaml:"session"`
	Workspace   Workspace      `yaml:"workspace"`
	Sync        SyncSpec       `yaml:"sync"`
	Ports       []PortMapping  `yaml:"ports"`
	SSH         SSHSpec        `yaml:"ssh"`
	Lifecycle   LifecycleSpec  `yaml:"lifecycle"`
	Sidecar     SidecarSpec    `yaml:"sidecar"`
	PodTemplate PodTemplateRef `yaml:"podTemplate"`
}

type SidecarSpec struct {
	Image string `yaml:"image"`
}

type SessionSpec struct {
	DefaultNameTemplate string `yaml:"defaultNameTemplate"`
	TTLHours            int    `yaml:"ttlHours"`
	IdleTimeoutMinutes  int    `yaml:"idleTimeoutMinutes"`
	Shareable           bool   `yaml:"shareable"`
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
	Paths         []string      `yaml:"paths"`
	Exclude       []string      `yaml:"exclude"`
	RemoteExclude []string      `yaml:"remoteExclude"`
	Engine        string        `yaml:"engine"`
	Syncthing     SyncthingSpec `yaml:"syncthing"`
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

type LifecycleSpec struct {
	PostCreate string `yaml:"postCreate"`
	PreStop    string `yaml:"preStop"`
}

type SSHSpec struct {
	User              string `yaml:"user"`
	RemotePort        int    `yaml:"remotePort"`
	PrivateKeyPath    string `yaml:"privateKeyPath"`
	AutoDetectPorts   *bool  `yaml:"autoDetectPorts"`
	PersistentSession *bool  `yaml:"persistentSession"`
	KeepAliveInterval int    `yaml:"keepAliveIntervalSeconds"`
	KeepAliveTimeout  int    `yaml:"keepAliveTimeoutSeconds"`
}

func (d *DevEnvironment) SetDefaults() {
	if d == nil {
		return
	}
	if d.Spec.Namespace == "" {
		d.Spec.Namespace = "default"
	}
	if d.Spec.Sync.Engine == "" {
		d.Spec.Sync.Engine = "syncthing"
	}
	if d.Spec.Sync.Syncthing.Version == "" {
		d.Spec.Sync.Syncthing.Version = DefaultSyncthingVersion
	}
	if d.Spec.Sync.Syncthing.AutoInstall == nil {
		v := true
		d.Spec.Sync.Syncthing.AutoInstall = &v
	}
	if d.Spec.Sync.Syncthing.Image == "" {
		d.Spec.Sync.Syncthing.Image = DefaultSidecarImageForBinaryVersion(version.Version)
	}
	if d.Spec.SSH.User == "" {
		d.Spec.SSH.User = "root"
	}
	if d.Spec.SSH.RemotePort == 0 {
		d.Spec.SSH.RemotePort = 22
	}
	if d.Spec.SSH.AutoDetectPorts == nil {
		v := true
		d.Spec.SSH.AutoDetectPorts = &v
	}
	if d.Spec.SSH.KeepAliveInterval == 0 {
		d.Spec.SSH.KeepAliveInterval = 10
	}
	if d.Spec.SSH.KeepAliveTimeout == 0 {
		d.Spec.SSH.KeepAliveTimeout = 15
	}
	if d.Spec.Sidecar.Image == "" {
		d.Spec.Sidecar.Image = DefaultSidecarImageForBinaryVersion(version.Version)
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
	if d.Spec.Sync.Engine != "syncthing" {
		return fmt.Errorf("spec.sync.engine must be syncthing, got %q", d.Spec.Sync.Engine)
	}
	if d.Spec.Session.TTLHours < 0 {
		return errors.New("spec.session.ttlHours must be >= 0")
	}
	if d.Spec.Session.IdleTimeoutMinutes < 0 {
		return errors.New("spec.session.idleTimeoutMinutes must be >= 0")
	}
	if err := validateSyncPaths(d.Spec.Sync.Paths); err != nil {
		return err
	}
	if len(d.Spec.Sync.Paths) > 1 {
		return errors.New("spec.sync.paths must contain at most one mapping when engine is syncthing")
	}
	if err := validatePortMappings(d.Spec.Ports); err != nil {
		return err
	}
	if err := validatePortRange("spec.ssh.remotePort", d.Spec.SSH.RemotePort); err != nil {
		return err
	}
	if d.Spec.SSH.KeepAliveInterval <= 0 {
		return errors.New("spec.ssh.keepAliveIntervalSeconds must be > 0")
	}
	if d.Spec.SSH.KeepAliveTimeout <= 0 {
		return errors.New("spec.ssh.keepAliveTimeoutSeconds must be > 0")
	}
	if d.Spec.SSH.KeepAliveTimeout < d.Spec.SSH.KeepAliveInterval {
		return errors.New("spec.ssh.keepAliveTimeoutSeconds must be >= spec.ssh.keepAliveIntervalSeconds")
	}
	if strings.TrimSpace(d.Spec.Sidecar.Image) == "" {
		return errors.New("spec.sidecar.image is required")
	}
	return nil
}

func (s SSHSpec) PersistentSessionEnabled() bool {
	if s.PersistentSession == nil {
		return false
	}
	return *s.PersistentSession
}

func (s SyncthingSpec) AutoInstallEnabled() bool {
	if s.AutoInstall == nil {
		return true
	}
	return *s.AutoInstall
}

func validateSyncPaths(paths []string) error {
	for _, p := range paths {
		parts := strings.Split(p, ":")
		if len(parts) != 2 {
			return fmt.Errorf("spec.sync.paths entry %q must be local:remote", p)
		}
		local := strings.TrimSpace(parts[0])
		remote := strings.TrimSpace(parts[1])
		if local == "" || remote == "" {
			return fmt.Errorf("spec.sync.paths entry %q must have non-empty local and remote", p)
		}
	}
	return nil
}

func validatePortMappings(ports []PortMapping) error {
	usedLocal := map[int]struct{}{}
	for i, p := range ports {
		if p.Local == 0 && p.Remote == 0 {
			continue
		}
		if err := validatePortRange(fmt.Sprintf("spec.ports[%d].local", i), p.Local); err != nil {
			return err
		}
		if err := validatePortRange(fmt.Sprintf("spec.ports[%d].remote", i), p.Remote); err != nil {
			return err
		}
		if _, exists := usedLocal[p.Local]; exists {
			return fmt.Errorf("spec.ports has duplicate local port %d", p.Local)
		}
		usedLocal[p.Local] = struct{}{}
	}
	return nil
}

func validatePortRange(field string, port int) error {
	if port <= 0 || port > 65535 {
		return fmt.Errorf("%s must be 1-65535, got %d", field, port)
	}
	return nil
}

func DefaultSidecarImageForBinaryVersion(binaryVersion string) string {
	tag := strings.TrimSpace(binaryVersion)
	if tag == "" || tag == "unknown" || strings.Contains(tag, "dev") {
		tag = DefaultSidecarImageFallback
	}
	return DefaultSidecarImageRepository + ":" + tag
}
