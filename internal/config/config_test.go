package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/version"
	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	sigsyaml "sigs.k8s.io/yaml"
)

func validConfig() *DevEnvironment {
	return &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec: DevEnvSpec{
			Sync:     SyncSpec{Engine: "syncthing"},
			Session:  SessionSpec{},
			Workload: WorkloadSpec{Type: "pod", ManifestPath: "pod.yaml"},
		},
	}
}

func TestTemplateRefRoundTrip(t *testing.T) {
	cfg := DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Spec: DevEnvSpec{
			Template: &TemplateRef{
				Name: "pytorch-ddp",
				Vars: map[string]any{
					"numWorkers": 4,
					"baseImage":  "pytorch:latest",
				},
			},
		},
	}

	data, err := yaml.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "template:") || !strings.Contains(string(data), "name: pytorch-ddp") {
		t.Fatalf("expected template block in YAML, got:\n%s", string(data))
	}

	var parsed DevEnvironment
	if err := yaml.Unmarshal(data, &parsed); err != nil {
		t.Fatal(err)
	}
	if parsed.Spec.Template == nil || parsed.Spec.Template.Name != "pytorch-ddp" {
		t.Fatalf("unexpected template ref after round-trip: %+v", parsed.Spec.Template)
	}
}

func TestTemplateRefOmittedWhenNil(t *testing.T) {
	data, err := yaml.Marshal(DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Spec:       DevEnvSpec{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "template:") {
		t.Fatalf("expected no template block when nil, got:\n%s", string(data))
	}
}

func TestSetDefaults(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec:       DevEnvSpec{},
	}
	cfg.SetDefaults()

	if cfg.Spec.Namespace != "default" {
		t.Fatalf("namespace default not set: %q", cfg.Spec.Namespace)
	}
	if cfg.Spec.Sync.Engine != "syncthing" {
		t.Fatalf("sync engine default not set: %q", cfg.Spec.Sync.Engine)
	}
	if cfg.Spec.Workload.Type != "pod" {
		t.Fatalf("workload type default not set: %q", cfg.Spec.Workload.Type)
	}
	if cfg.Spec.Sync.Syncthing.Image != DefaultSidecarImageForBinaryVersion(version.Version) {
		t.Fatalf("sync image default not set: %q", cfg.Spec.Sync.Syncthing.Image)
	}
	if !cfg.Spec.Sync.Syncthing.AutoInstallEnabled() {
		t.Fatal("expected syncthing autoinstall default true")
	}
	if cfg.Spec.Sync.Syncthing.RescanIntervalSeconds != DefaultSyncthingRescanSeconds {
		t.Fatalf("expected syncthing rescan default %d, got %d", DefaultSyncthingRescanSeconds, cfg.Spec.Sync.Syncthing.RescanIntervalSeconds)
	}
	if cfg.Spec.Sync.Syncthing.RelaysEnabled {
		t.Fatal("expected syncthing relays to default disabled")
	}
	if cfg.Spec.Sync.Syncthing.Compression {
		t.Fatal("expected syncthing compression to default disabled")
	}
	if cfg.Spec.SSH.User != "root" {
		t.Fatalf("ssh user default not set: %+v", cfg.Spec.SSH)
	}
	if cfg.Spec.SSH.KeepAliveInterval != 10 || cfg.Spec.SSH.KeepAliveTimeout != 10 || cfg.Spec.SSH.KeepAliveCountMax != 30 {
		t.Fatalf("ssh keepalive defaults not set: %+v", cfg.Spec.SSH)
	}
	if cfg.Spec.SSH.AutoDetectPorts == nil || !*cfg.Spec.SSH.AutoDetectPorts {
		t.Fatal("expected ssh autoDetectPorts default true")
	}
	if cfg.Spec.SSH.ForwardAgent != nil {
		t.Fatal("expected ssh forwardAgent to remain nil (off by default)")
	}
	if cfg.Spec.Sidecar.Image == "" {
		t.Fatal("expected sidecar image default to be set")
	}
	if cfg.Spec.Sidecar.Image != DefaultSidecarImageForBinaryVersion(version.Version) {
		t.Fatalf("sidecar image default not set correctly: %q", cfg.Spec.Sidecar.Image)
	}
	if got := cfg.Spec.Sync.Syncthing.EffectiveVersioningDays(); got != DefaultSyncthingVersioningDays {
		t.Fatalf("expected versioning default %d days, got %d", DefaultSyncthingVersioningDays, got)
	}
}

func TestSyncthingVersioningDays(t *testing.T) {
	var s SyncthingSpec
	if got := s.EffectiveVersioningDays(); got != DefaultSyncthingVersioningDays {
		t.Fatalf("nil should resolve to default, got %d", got)
	}
	zero := 0
	s.VersioningDays = &zero
	if got := s.EffectiveVersioningDays(); got != 0 {
		t.Fatalf("0 should disable versioning, got %d", got)
	}
	seven := 7
	s.VersioningDays = &seven
	if got := s.EffectiveVersioningDays(); got != 7 {
		t.Fatalf("expected 7, got %d", got)
	}

	cfg := validConfig()
	neg := -1
	cfg.Spec.Sync.Syncthing.VersioningDays = &neg
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "versioningDays") {
		t.Fatalf("expected versioningDays validation error, got %v", err)
	}
}

func TestSetDefaultsAppliesAgentConventionDefaults(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Agents = []AgentSpec{
		{Name: "claude-code"},
		{Name: "codex"},
		{Name: "gemini"},
		{Name: "opencode"},
	}

	cfg.SetDefaults()

	if cfg.Spec.Agents[0].Auth != nil {
		t.Fatalf("expected no default claude auth config, got %#v", cfg.Spec.Agents[0].Auth)
	}
	if got := cfg.Spec.Agents[1].Auth.LocalPath; got != "~/.codex/auth.json" {
		t.Fatalf("expected codex local path default, got %q", got)
	}
	if cfg.Spec.Agents[2].Auth != nil {
		t.Fatalf("expected no default gemini auth config, got %#v", cfg.Spec.Agents[2].Auth)
	}
	if cfg.Spec.Agents[3].Auth != nil {
		t.Fatalf("expected no default opencode auth config, got %#v", cfg.Spec.Agents[3].Auth)
	}
}

func TestSetDefaultsAutoDetectPortsFalse(t *testing.T) {
	cfg := &DevEnvironment{
		APIVersion: "okdev.io/v1alpha1",
		Kind:       "DevEnvironment",
		Metadata:   Metadata{Name: "x"},
		Spec:       DevEnvSpec{},
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

func TestSSHSpecForwardAgentEnabled(t *testing.T) {
	var s SSHSpec
	if s.ForwardAgentEnabled() {
		t.Fatal("expected nil forwardAgent to default false")
	}
	v := true
	s.ForwardAgent = &v
	if !s.ForwardAgentEnabled() {
		t.Fatal("expected explicit forwardAgent=true to be enabled")
	}
}

func TestSSHSpecInterPodEnabled(t *testing.T) {
	var s SSHSpec
	if s.InterPodEnabled() {
		t.Fatal("expected nil interPod to default false")
	}
	v := true
	s.InterPod = &v
	if !s.InterPodEnabled() {
		t.Fatal("expected explicit interPod=true to be enabled")
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

func TestValidateSyncPathDirection(t *testing.T) {
	for _, valid := range []string{"", "bi", "up", "down"} {
		cfg := validConfig()
		cfg.Spec.Sync.Paths = []SyncPathSpec{{Local: ".", Remote: "/workspace", Direction: valid}}
		cfg.SetDefaults()
		if err := cfg.Validate(); err != nil {
			t.Fatalf("direction %q should be valid, got %v", valid, err)
		}
	}
	for _, invalid := range []string{"push", "pull", "sendonly", "both"} {
		cfg := validConfig()
		cfg.Spec.Sync.Paths = []SyncPathSpec{{Local: ".", Remote: "/workspace", Direction: invalid}}
		cfg.SetDefaults()
		if err := cfg.Validate(); err == nil {
			t.Fatalf("direction %q should be rejected", invalid)
		}
	}
}

func TestSyncPathSpecEffectiveDirection(t *testing.T) {
	var p SyncPathSpec
	if got := p.EffectiveDirection(); got != SyncDirectionBi {
		t.Fatalf("expected default bi, got %q", got)
	}
	p.Direction = " down "
	if got := p.EffectiveDirection(); got != SyncDirectionDown {
		t.Fatalf("expected trimmed down, got %q", got)
	}
}

func TestSyncPathSpecYAMLForms(t *testing.T) {
	// Compact string form and structured form must both decode; the string
	// form keeps direction empty (= bi).
	var spec SyncSpec
	raw := []byte("paths:\n  - .:/workspace\n  - local: ../collected\n    remote: /data/results\n    direction: down\n")
	if err := sigsyaml.Unmarshal(raw, &spec); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(spec.Paths) != 2 {
		t.Fatalf("expected 2 paths, got %+v", spec.Paths)
	}
	if spec.Paths[0].Local != "." || spec.Paths[0].Remote != "/workspace" || spec.Paths[0].Direction != "" {
		t.Fatalf("unexpected compact entry: %+v", spec.Paths[0])
	}
	if spec.Paths[1].Local != "../collected" || spec.Paths[1].Remote != "/data/results" || spec.Paths[1].Direction != "down" {
		t.Fatalf("unexpected structured entry: %+v", spec.Paths[1])
	}

	// Round-trip: plain mappings marshal back to the compact string form,
	// directional ones keep the structured form.
	out, err := sigsyaml.Marshal(spec.Paths)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	text := string(out)
	if !strings.Contains(text, ".:/workspace") {
		t.Fatalf("expected compact form in output, got %q", text)
	}
	if !strings.Contains(text, "direction: down") {
		t.Fatalf("expected structured form for directional entry, got %q", text)
	}

	// Malformed compact entries fail decoding.
	if err := sigsyaml.Unmarshal([]byte("paths:\n  - nocolon\n"), &spec); err == nil {
		t.Fatal("expected error for entry without colon")
	}
}

func TestValidateAllowsInterPodSSHToOverrideDisabledSidecars(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = "manifests/pytorchjob.yaml"
	enabled := true
	disabled := false
	cfg.Spec.SSH.InterPod = &enabled
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{
		{Path: "spec.pytorchReplicaSpecs.Master.template"},
		{Path: "spec.pytorchReplicaSpecs.Worker.template", Sidecar: &disabled},
	}
	cfg.SetDefaults()

	if err := cfg.Validate(); err != nil {
		t.Fatalf("expected interPod to allow disabled sidecars in config, got %v", err)
	}
}

func TestValidateRejectsRelativeShellPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.Shell = "bash"
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for relative shell path")
	}
}

func TestValidateAcceptsAbsoluteShellPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.Shell = "/bin/zsh"
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateAcceptsEmptyShellPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.SSH.Shell = ""
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateRejectsNegativeSyncthingRescanInterval(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Syncthing.RescanIntervalSeconds = -1
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestSetDefaultsPreservesExplicitSyncthingCompression(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Syncthing.Compression = true
	cfg.SetDefaults()
	if !cfg.Spec.Sync.Syncthing.Compression {
		t.Fatal("expected explicit syncthing compression to be preserved")
	}
}

func TestValidateRejectsUnknownAgent(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Agents = []AgentSpec{{Name: "cursor"}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected unknown agent validation error")
	}
}

func TestValidateRejectsDuplicateAgents(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Agents = []AgentSpec{{Name: "codex"}, {Name: "codex"}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected duplicate agent validation error")
	}
}

func TestValidateRejectsInvalidAgentEnv(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Agents = []AgentSpec{{Name: "claude-code", Auth: &AgentAuth{Env: "bad-name"}}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected invalid agent env validation error")
	}
}

func TestValidateAcceptsAgentLocalPathWithTilde(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Agents = []AgentSpec{{Name: "codex", Auth: &AgentAuth{LocalPath: "~/.codex/auth.json"}}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestValidateRejectsInvalidSyncPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []SyncPathSpec{{Local: "./local-only", Remote: ""}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateAllowsDisjointMultiplePaths(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []SyncPathSpec{
		{Local: "./a", Remote: "/workspace/a"},
		{Local: "./b", Remote: "/data/results", Direction: "down"},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("disjoint multi-path mappings should validate, got %v", err)
	}
}

func TestValidateRejectsOverlappingSyncPaths(t *testing.T) {
	cases := []struct {
		name  string
		paths []SyncPathSpec
	}{
		{"equal locals", []SyncPathSpec{{Local: ".", Remote: "/a"}, {Local: "./", Remote: "/b"}}},
		{"extra contains primary", []SyncPathSpec{{Local: "./results/deep", Remote: "/a"}, {Local: "./results", Remote: "/b"}}},
		{"extras nested in each other", []SyncPathSpec{{Local: ".", Remote: "/a"}, {Local: "./x", Remote: "/b"}, {Local: "./x/deep", Remote: "/c"}}},
		{"equal remotes", []SyncPathSpec{{Local: "./a", Remote: "/workspace"}, {Local: "./b", Remote: "/workspace/"}}},
		{"nested remotes", []SyncPathSpec{{Local: "./a", Remote: "/workspace"}, {Local: "./b", Remote: "/workspace/results"}}},
		{"nested remote under primary even with nested local", []SyncPathSpec{{Local: ".", Remote: "/workspace"}, {Local: "./results", Remote: "/workspace/results"}}},
	}
	for _, tc := range cases {
		cfg := validConfig()
		cfg.Spec.Sync.Paths = tc.paths
		cfg.SetDefaults()
		if err := cfg.Validate(); err == nil {
			t.Fatalf("%s: expected overlap rejection", tc.name)
		}
	}
}

func TestValidateAllowsLocalsNestedInsidePrimary(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []SyncPathSpec{
		{Local: ".", Remote: "/workspace"},
		{Local: "./results", Remote: "/data/results", Direction: "down"},
		{Local: "./datasets", Remote: "/data/sets", Direction: "up"},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("locals nested inside the primary root should validate, got %v", err)
	}
}

func TestNestedLocalSyncIgnorePatterns(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Sync.Paths = []SyncPathSpec{
		{Local: ".", Remote: "/workspace"},
		{Local: "./results", Remote: "/data/results"},
		{Local: "./nested/deep", Remote: "/data/deep"},
		{Local: "../outside", Remote: "/data/outside"}, // disjoint — no pattern
	}
	got := cfg.NestedLocalSyncIgnorePatterns()
	want := []string{"/nested/deep", "/results"}
	if len(got) != 2 || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("unexpected patterns: %v (want %v)", got, want)
	}
	cfg.Spec.Sync.Paths = cfg.Spec.Sync.Paths[:1]
	if got := cfg.NestedLocalSyncIgnorePatterns(); len(got) != 0 {
		t.Fatalf("single mapping must produce no patterns, got %v", got)
	}
}

func TestSyncRootsOverlapLexicalEdges(t *testing.T) {
	// Sibling with a shared name prefix is NOT overlap; mixed abs/relative
	// roots cannot be compared lexically and are treated as disjoint.
	if syncRootsOverlap("/data/a", "/data/ab") {
		t.Fatal("prefix sibling must not count as overlap")
	}
	if syncRootsOverlap(".", "/data") {
		t.Fatal("mixed abs/relative must be treated as disjoint")
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

func TestValidateRejectsInvalidPortDirection(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Ports = []PortMapping{{Name: "a", Local: 8080, Remote: 8080, Direction: "sideways"}}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateRejectsDuplicateReverseRemotePorts(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Ports = []PortMapping{
		{Name: "a", Local: 3000, Remote: 8080, Direction: PortDirectionReverse},
		{Name: "b", Local: 3001, Remote: 8080, Direction: PortDirectionReverse},
	}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error")
	}
}

func TestValidateAllowsDuplicateLocalPortsForReverseMappings(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Ports = []PortMapping{
		{Name: "a", Local: 3000, Remote: 8080, Direction: PortDirectionReverse},
		{Name: "b", Local: 3000, Remote: 8081, Direction: PortDirectionReverse},
	}
	cfg.SetDefaults()
	if err := cfg.Validate(); err != nil {
		t.Fatalf("unexpected validation error: %v", err)
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

func TestValidateRejectsLegacyWorkspace(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workspace = &LegacyWorkspace{MountPath: "/workspace"}
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for legacy spec.workspace")
	}
}

func TestEffectiveWorkspaceMountPathUsesInjectedManifest(t *testing.T) {
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifestPath := filepath.Join(manifestDir, "pytorchjob.yaml")
	manifest := `apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: demo
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: dev
              volumeMounts:
                - name: workspace
                  mountPath: /train
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := validConfig()
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = "pytorchjob.yaml"
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Master.template"}}

	configPath := filepath.Join(manifestDir, "okdev.yaml")
	if got := cfg.EffectiveWorkspaceMountPath(configPath); got != "/train" {
		t.Fatalf("expected manifest-derived workspace mount path /train, got %q", got)
	}
}

func TestEffectiveWorkspaceMountPathUsesRelativeManifestForFolderConfig(t *testing.T) {
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifestPath := filepath.Join(manifestDir, "pytorchjob.yaml")
	manifest := `apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: demo
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: dev
              volumeMounts:
                - name: workspace
                  mountPath: /train
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := validConfig()
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = "pytorchjob.yaml"
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Master.template"}}

	configPath := filepath.Join(manifestDir, "okdev.yaml")
	if got := cfg.EffectiveWorkspaceMountPath(configPath); got != "/train" {
		t.Fatalf("expected manifest-derived workspace mount path /train, got %q", got)
	}
}

func TestEffectiveWorkspaceMountPathHandlesGoTemplatePlaceholders(t *testing.T) {
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifestPath := filepath.Join(manifestDir, "pytorchjob.yaml")
	// Manifest contains Go template placeholders (like those generated by
	// `okdev init --workload pytorchjob`) that only get rendered at apply
	// time. The workspace mount path lookup must still succeed.
	manifest := `apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: {{ .WorkloadName }}
  labels:
    app.kubernetes.io/name: {{ .WorkloadName }}
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: pytorch
              volumeMounts:
                - name: workspace
                  mountPath: /workspace/a
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := validConfig()
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = "pytorchjob.yaml"
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Master.template"}}
	cfg.Spec.Workload.Attach.Container = "pytorch"

	configPath := filepath.Join(manifestDir, "okdev.yaml")
	if got := cfg.EffectiveWorkspaceMountPath(configPath); got != "/workspace/a" {
		t.Fatalf("expected /workspace/a despite template placeholders, got %q", got)
	}
}

func TestEffectiveWorkspaceMountPathFallsBackToProjectRootManifestForFolderConfig(t *testing.T) {
	tmp := t.TempDir()
	manifestDir := filepath.Join(tmp, ".okdev")
	if err := os.MkdirAll(manifestDir, 0o755); err != nil {
		t.Fatalf("mkdir manifest dir: %v", err)
	}
	manifestPath := filepath.Join(tmp, "pytorchjob.yaml")
	manifest := `apiVersion: kubeflow.org/v1
kind: PyTorchJob
metadata:
  name: demo
spec:
  pytorchReplicaSpecs:
    Master:
      template:
        spec:
          containers:
            - name: dev
              volumeMounts:
                - name: workspace
                  mountPath: /train
`
	if err := os.WriteFile(manifestPath, []byte(manifest), 0o644); err != nil {
		t.Fatalf("write manifest: %v", err)
	}

	cfg := validConfig()
	cfg.Spec.Workload.Type = "pytorchjob"
	cfg.Spec.Workload.ManifestPath = "pytorchjob.yaml"
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.pytorchReplicaSpecs.Master.template"}}

	configPath := filepath.Join(manifestDir, "okdev.yaml")
	if got := cfg.EffectiveWorkspaceMountPath(configPath); got != "/train" {
		t.Fatalf("expected manifest-derived workspace mount path /train, got %q", got)
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

func TestValidateRejectsInvalidWorkloadType(t *testing.T) {
	cfg := validConfig()
	// Set before SetDefaults: defaulting desugars spec.workload into
	// spec.workloads, and validation reads the profiles from there.
	cfg.Spec.Workload.Type = "statefulset"
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid workload type")
	}
}

func TestSetDefaultsJobInjectPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workload.Type = "job"
	cfg.Spec.Workload.ManifestPath = "job.yaml"
	cfg.SetDefaults()
	if len(cfg.Spec.Workload.Inject) != 1 || cfg.Spec.Workload.Inject[0].Path != "spec.template" {
		t.Fatalf("unexpected job inject defaults: %+v", cfg.Spec.Workload.Inject)
	}
}

func TestValidateJobRequiresManifestPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workload.Type = "job"
	cfg.Spec.Workload.ManifestPath = ""
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing job manifestPath")
	}
}

func TestValidateJobRejectsUnexpectedInjectPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workload.Type = "job"
	cfg.Spec.Workload.ManifestPath = "job.yaml"
	cfg.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: "spec.other"}}
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for invalid job inject path")
	}
}

func TestValidateGenericRequiresInjectPath(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Workload.Type = "generic"
	cfg.Spec.Workload.ManifestPath = "controller.yaml"
	cfg.Spec.Workload.Inject = nil
	cfg.SetDefaults()
	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for missing generic inject paths")
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

func TestPodWorkloadDefaultsToRootInjectPath(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.SetDefaults()
	if cfg.Spec.Workload.Type != "pod" {
		t.Fatalf("type = %q, want pod", cfg.Spec.Workload.Type)
	}
	inject := cfg.EffectiveWorkloadInject()
	if len(inject) != 1 || inject[0].Path != "" {
		t.Fatalf("inject = %+v, want a single root-path entry", inject)
	}
}

func TestValidateAcceptsRootInjectPathOnlyForPod(t *testing.T) {
	pod := &DevEnvironment{APIVersion: "okdev.io/v1alpha1", Kind: "DevEnvironment"}
	pod.Metadata.Name = "x"
	pod.Spec.Workload.ManifestPath = "pod.yaml"
	pod.SetDefaults()
	if err := pod.Validate(); err != nil {
		t.Fatalf("pod with a root inject path must validate: %v", err)
	}

	generic := &DevEnvironment{APIVersion: "okdev.io/v1alpha1", Kind: "DevEnvironment"}
	generic.Metadata.Name = "x"
	generic.Spec.Workload.Type = "generic"
	generic.Spec.Workload.ManifestPath = "deploy.yaml"
	generic.Spec.Workload.Inject = []WorkloadInjectSpec{{Path: ""}}
	generic.SetDefaults()
	if err := generic.Validate(); err == nil {
		t.Fatal("generic workloads must still reject an empty inject path")
	}
}

func TestValidateRejectsSpecVolumes(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.Volumes = []corev1.Volume{{
		Name:         "datasets",
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}
	cfg.SetDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("spec.volumes must be rejected")
	}
	var migErr *MigrationEligibleError
	if !errors.As(err, &migErr) {
		t.Fatalf("the error must be migration-eligible so commands print the hint, got %T", err)
	}
	if !strings.Contains(err.Error(), "okdev migrate") {
		t.Fatalf("the error must name the fix, got %v", err)
	}
}

func TestValidateRejectsInlinePodTemplate(t *testing.T) {
	cfg := validConfig()
	cfg.Spec.PodTemplate = &PodTemplateRef{
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "dev", Image: "alpine"}}},
	}
	cfg.SetDefaults()

	err := cfg.Validate()
	if err == nil {
		t.Fatal("spec.podTemplate must be rejected")
	}
	var migErr *MigrationEligibleError
	if !errors.As(err, &migErr) {
		t.Fatalf("the error must be migration-eligible so commands print the hint, got %T", err)
	}
	if !strings.Contains(err.Error(), "okdev migrate") {
		t.Fatalf("the error must name the fix, got %v", err)
	}
}

func TestSnapshotNoLongerCarriesPodTemplate(t *testing.T) {
	cfg := &DevEnvironment{}
	cfg.Spec.Workload.Type = "pod"
	cfg.Spec.Workload.ManifestPath = "pod.yaml"
	cfg.SetDefaults()

	snap := BuildWorkloadSnapshot(cfg, "/workspace", "dev", false, "", "", "pod.yaml", "")
	raw, err := snap.JSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(raw, "podTemplate") {
		t.Fatalf("the snapshot must not carry podTemplate:\n%s", raw)
	}
}

// A pod manifest is injected at its own root, so it declares no inject path.
// Resolving the mount path must still find the dev container there — otherwise
// a project with a non-default --sync-remote silently reverts to /workspace.
func TestEffectiveWorkspaceMountPathReadsAPodManifestWithNoInjectPath(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, ".okdev", "okdev.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	manifest := `apiVersion: v1
kind: Pod
metadata:
  name: '{{ .WorkloadName }}'
spec:
  containers:
    - name: dev
      image: alpine
      volumeMounts:
        - name: workspace
          mountPath: /work
`
	if err := os.WriteFile(filepath.Join(dir, ".okdev", "pod.yaml"), []byte(manifest), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg := validConfig()
	cfg.Spec.Workload.Type = "pod"
	cfg.Spec.Workload.ManifestPath = "pod.yaml"
	cfg.SetDefaults()

	if got := cfg.EffectiveWorkspaceMountPath(cfgPath); got != "/work" {
		t.Fatalf("EffectiveWorkspaceMountPath = %q, want /work", got)
	}
}
