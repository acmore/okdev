package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/version"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/yaml"
)

// MigrationEligibleError wraps validation errors that can be fixed by okdev migrate.
type MigrationEligibleError struct {
	Err error
}

func (e *MigrationEligibleError) Error() string { return e.Err.Error() }
func (e *MigrationEligibleError) Unwrap() error { return e.Err }

const (
	DefaultSyncthingVersion       = "v1.29.7"
	DefaultSyncthingRescanSeconds = 300
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
	Agents      []AgentSpec      `yaml:"agents,omitempty"`
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
	PreservePaths []string      `yaml:"preservePaths"`
	Engine        string        `yaml:"engine"`
	Syncthing     SyncthingSpec `yaml:"syncthing"`
}

type SyncthingSpec struct {
	Version               string `yaml:"version"`
	AutoInstall           *bool  `yaml:"autoInstall"`
	Image                 string `yaml:"image"`
	RescanIntervalSeconds int    `yaml:"rescanIntervalSeconds"`
	WatcherDelaySeconds   int    `yaml:"watcherDelaySeconds"`
	RelaysEnabled         bool   `yaml:"relaysEnabled"`
	Compression           bool   `yaml:"compression"`
}

type PortMapping struct {
	Name      string `yaml:"name"`
	Local     int    `yaml:"local"`
	Remote    int    `yaml:"remote"`
	Direction string `yaml:"direction,omitempty"`
}

const (
	PortDirectionForward = "forward"
	PortDirectionReverse = "reverse"
)

type LifecycleSpec struct {
	PostCreate string `yaml:"postCreate"`
	PostSync   string `yaml:"postSync"`
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

type AgentSpec struct {
	Name string     `yaml:"name"`
	Auth *AgentAuth `yaml:"auth,omitempty"`
}

type AgentAuth struct {
	Env       string `yaml:"env,omitempty"`
	LocalPath string `yaml:"localPath,omitempty"`
}

var agentEnvVarPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

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
	if d.Spec.Sync.Syncthing.RescanIntervalSeconds == 0 {
		d.Spec.Sync.Syncthing.RescanIntervalSeconds = DefaultSyncthingRescanSeconds
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
	for i := range d.Spec.Agents {
		name := strings.TrimSpace(d.Spec.Agents[i].Name)
		d.Spec.Agents[i].Name = name
		spec, ok := agentcatalog.Lookup(name)
		if !ok {
			continue
		}
		if d.Spec.Agents[i].Auth == nil && spec.DefaultAuthEnv == "" && spec.DefaultLocalPath == "" {
			continue
		}
		if d.Spec.Agents[i].Auth == nil {
			d.Spec.Agents[i].Auth = &AgentAuth{}
		}
		if d.Spec.Agents[i].Auth.Env == "" && spec.DefaultAuthEnv != "" {
			d.Spec.Agents[i].Auth.Env = spec.DefaultAuthEnv
		}
		if d.Spec.Agents[i].Auth.LocalPath == "" && spec.DefaultLocalPath != "" {
			d.Spec.Agents[i].Auth.LocalPath = spec.DefaultLocalPath
		}
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
	if d.Spec.Sync.Syncthing.RescanIntervalSeconds < 0 {
		return errors.New("spec.sync.syncthing.rescanIntervalSeconds must be >= 0")
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
	if err := validateAgents(d.Spec.Agents); err != nil {
		return err
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

func (d *DevEnvironment) EffectiveWorkspaceMountPath(configPath string) string {
	if d == nil {
		return DefaultWorkspacePath
	}
	switch strings.TrimSpace(d.Spec.Workload.Type) {
	case "", "pod":
		return d.WorkspaceMountPath()
	default:
		if path := d.workspaceMountPathFromManifest(configPath); strings.TrimSpace(path) != "" {
			return path
		}
		return d.WorkspaceMountPath()
	}
}

func (d *DevEnvironment) workspaceMountPathFromManifest(configPath string) string {
	resolvedManifestPath := ResolveWorkloadManifestPath(configPath, strings.TrimSpace(d.Spec.Workload.ManifestPath))
	if resolvedManifestPath == "" || configPath == "" {
		return ""
	}
	raw, err := os.ReadFile(resolvedManifestPath)
	if err != nil {
		return ""
	}
	var obj map[string]any
	if err := yaml.Unmarshal(raw, &obj); err != nil {
		return ""
	}
	targetContainer := strings.TrimSpace(d.Spec.Workload.Attach.Container)
	if targetContainer == "" {
		targetContainer = "dev"
	}
	for _, inject := range d.Spec.Workload.Inject {
		templateMap, ok := resolveObjectPath(obj, inject.Path)
		if !ok {
			continue
		}
		template, err := decodeTemplateSpec(templateMap)
		if err != nil {
			continue
		}
		if path := workspaceMountPathForContainer(template.Spec.Containers, targetContainer); path != "" {
			return path
		}
	}
	return ""
}

func resolveObjectPath(root map[string]any, path string) (map[string]any, bool) {
	current := root
	for _, part := range strings.Split(strings.TrimSpace(path), ".") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		next, ok := current[part]
		if !ok {
			return nil, false
		}
		child, ok := next.(map[string]any)
		if !ok {
			return nil, false
		}
		current = child
	}
	return current, true
}

func decodeTemplateSpec(src map[string]any) (corev1.PodTemplateSpec, error) {
	raw, err := yaml.Marshal(src)
	if err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	var template corev1.PodTemplateSpec
	if err := yaml.Unmarshal(raw, &template); err != nil {
		return corev1.PodTemplateSpec{}, err
	}
	return template, nil
}

func workspaceMountPathForContainer(containers []corev1.Container, name string) string {
	index := -1
	for i := range containers {
		if containers[i].Name == name {
			index = i
			break
		}
	}
	if index == -1 && len(containers) > 0 {
		index = 0
	}
	if index == -1 {
		return ""
	}
	for _, vm := range containers[index].VolumeMounts {
		if vm.Name == DefaultWorkspaceName && strings.TrimSpace(vm.MountPath) != "" {
			return vm.MountPath
		}
	}
	return ""
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
	usedRemote := map[int]struct{}{}
	for i, p := range ports {
		if p.Local == 0 && p.Remote == 0 {
			continue
		}
		direction := normalizePortDirection(p.Direction)
		switch direction {
		case PortDirectionForward, PortDirectionReverse:
		default:
			return fmt.Errorf("spec.ports[%d].direction must be %q or %q, got %q", i, PortDirectionForward, PortDirectionReverse, p.Direction)
		}
		if err := validatePortRange(fmt.Sprintf("spec.ports[%d].local", i), p.Local); err != nil {
			return err
		}
		if err := validatePortRange(fmt.Sprintf("spec.ports[%d].remote", i), p.Remote); err != nil {
			return err
		}
		if direction == PortDirectionReverse {
			if _, exists := usedRemote[p.Remote]; exists {
				return fmt.Errorf("spec.ports has duplicate reverse remote port %d", p.Remote)
			}
			usedRemote[p.Remote] = struct{}{}
			continue
		}
		if _, exists := usedLocal[p.Local]; exists {
			return fmt.Errorf("spec.ports has duplicate local port %d", p.Local)
		}
		usedLocal[p.Local] = struct{}{}
	}
	return nil
}

func normalizePortDirection(direction string) string {
	switch strings.ToLower(strings.TrimSpace(direction)) {
	case "", PortDirectionForward:
		return PortDirectionForward
	case PortDirectionReverse:
		return PortDirectionReverse
	default:
		return strings.ToLower(strings.TrimSpace(direction))
	}
}

func (p PortMapping) EffectiveDirection() string {
	return normalizePortDirection(p.Direction)
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

func validateAgents(agents []AgentSpec) error {
	seen := map[string]struct{}{}
	for i, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			return fmt.Errorf("spec.agents[%d].name must be one of %s, got %q", i, strings.Join(agentcatalog.SupportedNames(), ", "), agent.Name)
		}
		if _, exists := seen[spec.Name]; exists {
			return fmt.Errorf("spec.agents has duplicate name %q", spec.Name)
		}
		seen[spec.Name] = struct{}{}
		if agent.Auth == nil {
			continue
		}
		if env := strings.TrimSpace(agent.Auth.Env); env != "" && !agentEnvVarPattern.MatchString(env) {
			return fmt.Errorf("spec.agents[%d].auth.env must be a valid env var name, got %q", i, agent.Auth.Env)
		}
		if path := strings.TrimSpace(agent.Auth.LocalPath); path != "" {
			if _, err := ResolveLocalAgentPath(path); err != nil {
				return fmt.Errorf("spec.agents[%d].auth.localPath: %w", i, err)
			}
		}
	}
	return nil
}

func ResolveLocalAgentPath(path string) (string, error) {
	trimmed := strings.TrimSpace(path)
	if trimmed == "" {
		return "", errors.New("path is empty")
	}
	switch {
	case trimmed == "~":
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return home, nil
	case strings.HasPrefix(trimmed, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("resolve home directory: %w", err)
		}
		return filepath.Join(home, strings.TrimPrefix(trimmed, "~/")), nil
	default:
		abs, err := filepath.Abs(trimmed)
		if err != nil {
			return "", fmt.Errorf("resolve absolute path: %w", err)
		}
		return abs, nil
	}
}
