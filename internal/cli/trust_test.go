package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/acmore/okdev/internal/config"
	"github.com/spf13/cobra"
)

func trustTestConfig(t *testing.T, project string) (*config.DevEnvironment, string) {
	t.Helper()
	cfgPath := filepath.Join(project, ".okdev", "okdev.yaml")
	if err := os.MkdirAll(filepath.Dir(cfgPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("apiVersion: okdev.io/v1alpha1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := &config.DevEnvironment{}
	cfg.Spec.Sync.Paths = []config.SyncPathSpec{{Local: filepath.Join(project, "src"), Remote: "/workspace"}}
	return cfg, cfgPath
}

func gateCmd() (*cobra.Command, *bytes.Buffer) {
	cmd := &cobra.Command{}
	var errOut bytes.Buffer
	cmd.SetIn(strings.NewReader(""))
	cmd.SetErr(&errOut)
	cmd.SetOut(&errOut)
	return cmd, &errOut
}

func TestLocalCapabilitiesOnlyListsReachBeyondTheProject(t *testing.T) {
	project := t.TempDir()
	t.Chdir(project)
	cfg, cfgPath := trustTestConfig(t, project)
	if caps, err := localCapabilities(cfg, cfgPath); err != nil || len(caps) != 0 {
		t.Fatalf("an in-project config needs no approval, got %v %v", caps, err)
	}

	home, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	canonicalProject, err := filepath.EvalSymlinks(project)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Spec.Sync.Paths = append(cfg.Spec.Sync.Paths, config.SyncPathSpec{Local: home, Remote: "/home"})
	cfg.Spec.Sync.Paths = append(cfg.Spec.Sync.Paths, config.SyncPathSpec{Local: "../sibling", Remote: "/sibling"})
	cfg.Spec.Agents = []config.AgentSpec{{Name: "codex", Auth: &config.AgentAuth{LocalPath: "~/.codex/auth.json"}}}
	cfg.Spec.Ports = []config.PortMapping{{Local: 5432, Remote: 5432, Direction: config.PortDirectionReverse}, {Local: 8080, Remote: 8080}}
	enabled := true
	cfg.Spec.SSH.ForwardAgent = &enabled

	caps, err := localCapabilities(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	project = canonicalProject
	var texts []string
	for _, c := range caps {
		texts = append(texts, c.text)
	}
	joined := strings.Join(texts, "\n")
	for _, want := range []string{
		"sync " + home + " (outside the project",
		"sync " + filepath.Join(filepath.Dir(project), "sibling"),
		"upload codex credentials",
		"localhost:5432",
		"forward this machine's SSH agent",
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if strings.Contains(joined, "8080") || strings.Contains(joined, filepath.Join(project, "src")) {
		t.Fatalf("forward ports and in-project roots are not local reach:\n%s", joined)
	}
}

func TestRequireConfigTrustGatesUntilApproved(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	_, cfgPath := trustTestConfig(t, project)
	dataDir := t.TempDir()
	if err := os.WriteFile(cfgPath, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: demo
  workload:
    type: pod
    manifestPath: pod.yaml
  sync:
    paths:
      - "src:/workspace"
      - "`+dataDir+`:/data"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}

	cmd, _ := gateCmd()
	err = requireConfigTrust(cmd, cfg, cfgPath, allCapabilityKinds...)
	if err == nil || !strings.Contains(err.Error(), "okdev trust --config") {
		t.Fatalf("non-interactive gate must refuse and name okdev trust, got %v", err)
	}
	// A command that does not use sync is not gated by it.
	if err := requireConfigTrust(cmd, cfg, cfgPath, capReverseForward, capAgentForward); err != nil {
		t.Fatalf("unrelated capability gated: %v", err)
	}

	trust := newTrustCmd(&Options{ConfigPath: cfgPath})
	trust.SetArgs([]string{"--yes"})
	var out bytes.Buffer
	trust.SetOut(&out)
	trust.SetIn(strings.NewReader(""))
	if err := trust.Execute(); err != nil || !strings.Contains(out.String(), "Approved.") {
		t.Fatalf("okdev trust --yes: %v %s", err, out.String())
	}
	if err := requireConfigTrust(cmd, cfg, cfgPath, allCapabilityKinds...); err != nil {
		t.Fatalf("approved config still gated: %v", err)
	}

	// Unrelated edits keep the approval; new local reach asks again.
	cfg.Spec.Namespace = "elsewhere"
	if err := requireConfigTrust(cmd, cfg, cfgPath, allCapabilityKinds...); err != nil {
		t.Fatalf("unrelated edit dropped approval: %v", err)
	}
	enabled := true
	cfg.Spec.SSH.ForwardAgent = &enabled
	if err := requireConfigTrust(cmd, cfg, cfgPath, allCapabilityKinds...); err == nil || !strings.Contains(err.Error(), "SSH agent") {
		t.Fatalf("new capability must be approved, got %v", err)
	}

	// Approval is per config path: a copy elsewhere starts unapproved.
	otherProject := t.TempDir()
	other, otherPath := trustTestConfig(t, otherProject)
	other.Spec.Sync.Paths = cfg.Spec.Sync.Paths
	if err := requireConfigTrust(cmd, other, otherPath, capSyncOutsideProject); err == nil {
		t.Fatal("approval leaked to another config path")
	}
}

func TestTrustCommandNeedsNoApprovalForInProjectConfig(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	_, cfgPath := trustTestConfig(t, project)
	if err := os.WriteFile(cfgPath, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: demo
  workload:
    type: pod
    manifestPath: pod.yaml
  sync:
    paths:
      - ".:/workspace"
`), 0o644); err != nil {
		t.Fatal(err)
	}
	trust := newTrustCmd(&Options{ConfigPath: cfgPath})
	var out bytes.Buffer
	trust.SetOut(&out)
	trust.SetIn(strings.NewReader(""))
	if err := trust.Execute(); err != nil || !strings.Contains(out.String(), "no approval needed") {
		t.Fatalf("expected no approval needed: %v %s", err, out.String())
	}
}

func TestConfigTrustAgreesAcrossSymlinkedPaths(t *testing.T) {
	// macOS /tmp is a symlink to /private/tmp: the same project reached by
	// either spelling must be one project with one approval.
	t.Setenv("HOME", t.TempDir())
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}
	t.Chdir(real)
	_, cfgPath := trustTestConfig(t, real)
	if err := os.WriteFile(cfgPath, []byte(`apiVersion: okdev.io/v1alpha1
kind: DevEnvironment
metadata:
  name: demo
spec:
  namespace: demo
  workload:
    type: pod
    manifestPath: pod.yaml
  sync:
    paths:
      - "`+filepath.Join(link, "data")+`:/workspace"
  ssh:
    forwardAgent: true
`), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, _, err := config.Load(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	caps, err := localCapabilities(cfg, cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range caps {
		if c.kind == capSyncOutsideProject {
			t.Fatalf("in-project root reached through a symlink reported as outside: %s", c.text)
		}
	}
	// Approve through the symlinked spelling, check through the real one.
	trust := newTrustCmd(&Options{ConfigPath: filepath.Join(link, ".okdev", "okdev.yaml")})
	trust.SetArgs([]string{"--yes"})
	trust.SetOut(&bytes.Buffer{})
	trust.SetIn(strings.NewReader(""))
	if err := trust.Execute(); err != nil {
		t.Fatal(err)
	}
	cmd, _ := gateCmd()
	if err := requireConfigTrust(cmd, cfg, cfgPath, allCapabilityKinds...); err != nil {
		t.Fatalf("approval keyed by the other spelling: %v", err)
	}
}

func TestConfigTrustTracksAgentSymlinkTarget(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	project := t.TempDir()
	t.Chdir(project)
	cfg, cfgPath := trustTestConfig(t, project)
	outside := t.TempDir()
	safe := filepath.Join(outside, "safe")
	secret := filepath.Join(outside, "secret")
	for _, path := range []string{safe, secret} {
		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	link := filepath.Join(project, "auth")
	if err := os.Symlink(safe, link); err != nil {
		t.Fatal(err)
	}
	cfg.Spec.Agents = []config.AgentSpec{{Name: "codex", Auth: &config.AgentAuth{LocalPath: link}}}
	caps, err := localCapabilities(cfg, cfgPath)
	if err != nil || len(caps) != 1 {
		t.Fatalf("capabilities: %v (%v)", caps, err)
	}
	absPath, err := canonicalPath(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := approveConfigTrust(absPath, caps); err != nil {
		t.Fatal(err)
	}
	cmd, _ := gateCmd()
	if err := requireConfigTrust(cmd, cfg, cfgPath, capCredentialUpload); err != nil {
		t.Fatalf("approved credential refused: %v", err)
	}
	if err := os.Remove(link); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(secret, link); err != nil {
		t.Fatal(err)
	}
	if err := requireConfigTrust(cmd, cfg, cfgPath, capCredentialUpload); err == nil {
		t.Fatal("retargeted credential was still approved")
	}
	canonicalSecret, err := filepath.EvalSymlinks(secret)
	if err != nil {
		t.Fatal(err)
	}
	if resolved, found, err := resolveAgentLocalAuth(link); err != nil || !found || resolved != canonicalSecret {
		t.Fatalf("upload path = %q, found=%v, err=%v", resolved, found, err)
	}
}
