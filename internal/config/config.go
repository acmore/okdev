package config

import (
	"errors"
	"fmt"
	"strings"

	"github.com/acmore/okdev/internal/version"
	corev1 "k8s.io/api/core/v1"
)

// MigrationEligibleError wraps validation errors that can be fixed by okdev migrate.
type MigrationEligibleError struct {
	Err error
}

func (e *MigrationEligibleError) Error() string { return e.Err.Error() }
func (e *MigrationEligibleError) Unwrap() error { return e.Err }

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
	Namespace   string           `yaml:"namespace"`
	KubeContext string           `yaml:"kubeContext"`
	Session     SessionSpec      `yaml:"session"`
	Workload    WorkloadSpec     `yaml:"workload"`
	Volumes     []corev1.Volume  `yaml:"volumes"`
	Workspace   *LegacyWorkspace `yaml:"workspace,omitempty"`
	Sync        SyncSpec         `yaml:"sync"`
	Ports       []PortMapping    `yaml:"ports"`
	SSH         SSHSpec          `yaml:"ssh"`
	Lifecycle   LifecycleSpec    `yaml:"lifecycle"`
	Sidecar     SidecarSpec      `yaml:"sidecar"`
	PodTemplate PodTemplateRef   `yaml:"podTemplate"`
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

type WorkloadSpec struct {
	Type         string               `yaml:"type"`
	ManifestPath string               `yaml:"manifestPath,omitempty"`
	Inject       []WorkloadInjectSpec `yaml:"inject,omitempty"`
	Attach       WorkloadAttachSpec   `yaml:"attach,omitempty"`
}

type WorkloadInjectSpec struct {
	Path       string `yaml:"path,omitempty"`
	Sidecar    *bool  `yaml:"sidecar,omitempty"`
	Attachable *bool  `yaml:"attachable,omitempty"`
}

type WorkloadAttachSpec struct {
	Container string `yaml:"container,omitempty"`
}

const (
	DefaultWorkspacePath = "/workspace"
	DefaultWorkspaceName = "workspace"
)

// LegacyWorkspace exists only to produce a clear migration error for removed config.
type LegacyWorkspace struct {
	MountPath string            `yaml:"mountPath"`
	PVC       map[string]string `yaml:"pvc"`
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
	PrivateKeyPath    string `yaml:"privateKeyPath"`
	AutoDetectPorts   *bool  `yaml:"autoDetectPorts"`
	PersistentSession *bool  `yaml:"persistentSession"`
	KeepAliveInterval int    `yaml:"keepAliveIntervalSeconds"`
	KeepAliveTimeout  int    `yaml:"keepAliveTimeoutSeconds"`
	KeepAliveCountMax int    `yaml:"keepAliveCountMax"`
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
	if strings.TrimSpace(d.Spec.Workload.Type) == "" {
		d.Spec.Workload.Type = "pod"
	}
	if d.Spec.Workload.Type == "job" && len(d.Spec.Workload.Inject) == 0 {
		d.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.template"}}
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
	if d.Spec.SSH.AutoDetectPorts == nil {
		v := true
		d.Spec.SSH.AutoDetectPorts = &v
	}
	if d.Spec.SSH.KeepAliveInterval == 0 {
		d.Spec.SSH.KeepAliveInterval = 10
	}
	if d.Spec.SSH.KeepAliveTimeout == 0 {
		d.Spec.SSH.KeepAliveTimeout = 10
	}
	if d.Spec.SSH.KeepAliveCountMax == 0 {
		d.Spec.SSH.KeepAliveCountMax = 30
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
	if d.Spec.Workspace != nil {
		return &MigrationEligibleError{Err: errors.New("spec.workspace is removed; use spec.volumes (k8s Volume) and podTemplate.spec.containers[*].volumeMounts, or run \"okdev migrate\" to automatically update your config")}
	}
	switch strings.TrimSpace(d.Spec.Workload.Type) {
	case "", "pod", "job", "pytorchjob", "generic":
	default:
		return fmt.Errorf("spec.workload.type must be one of pod, job, pytorchjob, generic, got %q", d.Spec.Workload.Type)
	}
	switch strings.TrimSpace(d.Spec.Workload.Type) {
	case "job", "pytorchjob", "generic":
		if strings.TrimSpace(d.Spec.Workload.ManifestPath) == "" {
			return fmt.Errorf("spec.workload.manifestPath is required when spec.workload.type=%q", d.Spec.Workload.Type)
		}
	}
	for i, inject := range d.Spec.Workload.Inject {
		if strings.TrimSpace(inject.Path) == "" {
			return fmt.Errorf("spec.workload.inject[%d].path is required", i)
		}
		if inject.Attachable != nil && *inject.Attachable && inject.Sidecar != nil && !*inject.Sidecar {
			return fmt.Errorf("spec.workload.inject[%d]: attachable=true requires sidecar=true", i)
		}
	}
	if d.Spec.Workload.Type == "job" {
		for i, inject := range d.Spec.Workload.Inject {
			if strings.TrimSpace(inject.Path) != "spec.template" {
				return fmt.Errorf("spec.workload.inject[%d].path must be spec.template when spec.workload.type=job", i)
			}
		}
	}
	if (d.Spec.Workload.Type == "generic" || d.Spec.Workload.Type == "pytorchjob") && len(d.Spec.Workload.Inject) == 0 {
		return fmt.Errorf("spec.workload.inject is required when spec.workload.type=%q", d.Spec.Workload.Type)
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
	if d.Spec.SSH.KeepAliveInterval <= 0 {
		return errors.New("spec.ssh.keepAliveIntervalSeconds must be > 0")
	}
	if d.Spec.SSH.KeepAliveTimeout <= 0 {
		return errors.New("spec.ssh.keepAliveTimeoutSeconds must be > 0")
	}
	if d.Spec.SSH.KeepAliveTimeout < d.Spec.SSH.KeepAliveInterval {
		return errors.New("spec.ssh.keepAliveTimeoutSeconds must be >= spec.ssh.keepAliveIntervalSeconds")
	}
	if d.Spec.SSH.KeepAliveCountMax <= 0 {
		return errors.New("spec.ssh.keepAliveCountMax must be > 0")
	}
	if strings.TrimSpace(d.Spec.Sidecar.Image) == "" {
		return errors.New("spec.sidecar.image is required")
	}
	return nil
}

func (s SSHSpec) PersistentSessionEnabled() bool {
	if s.PersistentSession == nil {
		return true
	}
	return *s.PersistentSession
}

func (s SyncthingSpec) AutoInstallEnabled() bool {
	if s.AutoInstall == nil {
		return true
	}
	return *s.AutoInstall
}

func (d *DevEnvironment) EffectiveVolumes() []corev1.Volume {
	out := make([]corev1.Volume, 0, len(d.Spec.Volumes)+1)
	hasWorkspace := false
	for _, v := range d.Spec.Volumes {
		if v.Name == DefaultWorkspaceName {
			hasWorkspace = true
		}
		out = append(out, v)
	}
	if !hasWorkspace {
		out = append(out, corev1.Volume{
			Name: DefaultWorkspaceName,
			VolumeSource: corev1.VolumeSource{
				EmptyDir: &corev1.EmptyDirVolumeSource{},
			},
		})
	}
	return out
}

func (d *DevEnvironment) WorkspaceMountPath() string {
	for _, c := range d.Spec.PodTemplate.Spec.Containers {
		if c.Name != "dev" {
			continue
		}
		for _, vm := range c.VolumeMounts {
			if vm.Name == DefaultWorkspaceName && strings.TrimSpace(vm.MountPath) != "" {
				return vm.MountPath
			}
		}
	}
	return DefaultWorkspacePath
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
