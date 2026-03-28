package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
)

type fakeAgentExecClient struct {
	mu        sync.Mutex
	scripts   []string
	copyCalls []string
	outputs   map[string][]byte
	results   map[string]error
}

const fakeAgentNPMDetectKey = "__agent_npm_detect__"

func looksLikeAgentNPMDetectScript(script string) bool {
	return strings.Contains(script, "echo install:${installer}") &&
		strings.Contains(script, "echo unavailable:none") &&
		strings.Contains(script, "command -v bash") &&
		strings.Contains(script, "command -v curl")
}

func (f *fakeAgentExecClient) ExecShInContainer(_ context.Context, _, _, _, script string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.scripts = append(f.scripts, script)
	switch script {
	case "npm install -g @openai/codex":
	case "npm install -g @anthropic-ai/claude-code":
	case "npm install -g @google/gemini-cli":
	case "npm install -g opencode-ai":
	}
	switch {
	case strings.Contains(script, "binary='codex'"):
		delete(f.results, "command -v codex >/dev/null 2>&1")
	case strings.Contains(script, "binary='claude'"):
		delete(f.results, "command -v claude >/dev/null 2>&1")
	case strings.Contains(script, "binary='gemini'"):
		delete(f.results, "command -v gemini >/dev/null 2>&1")
	case strings.Contains(script, "binary='opencode'"):
		delete(f.results, "command -v opencode >/dev/null 2>&1")
	}
	if strings.HasPrefix(script, "export OKDEV_NPM_INSTALLER=") && strings.Contains(script, "__OKDEV_NPM_STATUS__=installed:") {
		delete(f.results, "command -v npm >/dev/null 2>&1")
		delete(f.results, "node -p 'process.versions.node.split(\".\")[0]'")
		if strings.Contains(script, "nvm install 20") {
			return []byte("__OKDEV_NPM_STATUS__=installed:nvm\n"), nil
		}
		return []byte("__OKDEV_NPM_STATUS__=installed:none\n"), nil
	}
	if looksLikeAgentNPMDetectScript(script) {
		if out, ok := f.outputs[fakeAgentNPMDetectKey]; ok {
			return out, nil
		}
		if err, ok := f.results[fakeAgentNPMDetectKey]; ok {
			return nil, err
		}
	}
	if err, ok := f.results[script]; ok {
		return nil, err
	}
	if out, ok := f.outputs[script]; ok {
		return out, nil
	}
	return nil, nil
}

func (f *fakeAgentExecClient) CopyToPodInContainer(_ context.Context, _, localPath, _, _, remotePath string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.copyCalls = append(f.copyCalls, localPath+"->"+remotePath)
	return nil
}

func TestConfiguredAgentStatusRows(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v claude >/dev/null 2>&1":   nil,
			"command -v codex >/dev/null 2>&1":    errors.New("exit status 1"),
			"command -v gemini >/dev/null 2>&1":   nil,
			"command -v opencode >/dev/null 2>&1": nil,
			`test -f "$HOME"/'.codex/auth.json'`:  errors.New("exit status 1"),
		},
	}
	agents := []config.AgentSpec{
		{Name: "claude-code"},
		{Name: "codex", Auth: &config.AgentAuth{LocalPath: "~/.codex/auth.json"}},
		{Name: "gemini"},
		{Name: "opencode"},
	}

	rows, err := configuredAgentStatusRows(context.Background(), client, "default", "pod", "dev", agents)
	if err != nil {
		t.Fatalf("configuredAgentStatusRows: %v", err)
	}
	if len(rows) != 4 {
		t.Fatalf("expected 4 rows, got %d", len(rows))
	}
	if !rows[0].Installed || rows[1].Installed || !rows[2].Installed || !rows[3].Installed {
		t.Fatalf("unexpected install states: %#v", rows)
	}
	if rows[0].AuthEnv != "" || rows[0].AuthPath != "" || rows[1].AuthPath != "~/.codex/auth.json" || rows[2].AuthEnv != "" || rows[2].AuthPath != "" || rows[3].AuthEnv != "" || rows[3].AuthPath != "" {
		t.Fatalf("unexpected auth fields: %#v", rows)
	}
	if rows[0].AuthStaged || rows[1].AuthStaged || rows[2].AuthStaged || rows[3].AuthStaged {
		t.Fatalf("unexpected auth staged states: %#v", rows)
	}
}

func TestEnsureConfiguredAgentsInstalled(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v claude >/dev/null 2>&1":   nil,
			"command -v codex >/dev/null 2>&1":    errors.New("exit status 1"),
			"command -v gemini >/dev/null 2>&1":   errors.New("exit status 1"),
			"command -v opencode >/dev/null 2>&1": errors.New("exit status 1"),
			"command -v npm >/dev/null 2>&1":      nil,
			"npm install -g @openai/codex":        nil,
			"npm install -g @google/gemini-cli":   nil,
			"npm install -g opencode-ai":          nil,
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("20\n"),
		},
	}
	var warnings []string
	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "claude-code"}, {Name: "codex"}, {Name: "gemini"}, {Name: "opencode"}},
		func(format string, args ...any) { warnings = append(warnings, format) },
	)

	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", warnings)
	}
	if got := strings.Join(results, ", "); !strings.Contains(got, "claude-code: present") || !strings.Contains(got, "codex: installed") || !strings.Contains(got, "gemini: installed") || !strings.Contains(got, "opencode: installed") {
		t.Fatalf("unexpected install summary %q", got)
	}
}

func TestEnsureConfiguredAgentsInstalledBootstrapsNPMOnce(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v codex >/dev/null 2>&1":    errors.New("exit status 1"),
			"command -v gemini >/dev/null 2>&1":   errors.New("exit status 1"),
			"command -v opencode >/dev/null 2>&1": errors.New("exit status 1"),
			"npm install -g @openai/codex":        nil,
			"npm install -g @google/gemini-cli":   nil,
			"npm install -g opencode-ai":          nil,
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("12\n"),
			fakeAgentNPMDetectKey:                           []byte("install:nvm\n"),
		},
	}

	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}, {Name: "gemini"}, {Name: "opencode"}},
		func(string, ...any) {},
	)

	if got := strings.Join(results, ", "); strings.Count(got, "node/npm installed via nvm") != 1 {
		t.Fatalf("expected one npm bootstrap result, got %q", got)
	}
	detectCalls := 0
	for _, script := range client.scripts {
		if looksLikeAgentNPMDetectScript(script) {
			detectCalls++
		}
	}
	if detectCalls != 1 {
		t.Fatalf("expected one npm detect call, got %d (%#v)", detectCalls, client.scripts)
	}
}

func TestEnsureConfiguredAgentsInstalledWarnsWhenNpmMissing(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": errors.New("exit status 1"),
			"command -v npm >/dev/null 2>&1":   errors.New("exit status 1"),
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("0\n"),
			fakeAgentNPMDetectKey:                           []byte("unavailable:none\n"),
		},
	}
	var warnings []string
	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}},
		func(format string, args ...any) { warnings = append(warnings, format) },
	)

	if len(warnings) != 1 || !strings.Contains(warnings[0], "skipping %s install") {
		t.Fatalf("unexpected warnings %#v", warnings)
	}
	if len(results) != 1 || results[0] != "codex: skipped (npm missing)" {
		t.Fatalf("unexpected results %#v", results)
	}
}

func TestEnsureConfiguredAgentsInstalledReportsUnknownAgents(t *testing.T) {
	var warnings []string
	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		&fakeAgentExecClient{},
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "mystery-agent"}},
		func(format string, args ...any) { warnings = append(warnings, fmt.Sprintf(format, args...)) },
	)

	if len(warnings) != 1 || !strings.Contains(warnings[0], `unknown configured agent "mystery-agent"`) {
		t.Fatalf("unexpected warnings %#v", warnings)
	}
	if len(results) != 1 || results[0] != "mystery-agent: unsupported" {
		t.Fatalf("unexpected results %#v", results)
	}
}

func TestEnsureConfiguredAgentsInstalledBootstrapsNPM(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": errors.New("exit status 1"),
			"npm install -g @openai/codex":     nil,
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("12\n"),
			fakeAgentNPMDetectKey:                           []byte("install:nvm\n"),
		},
	}
	var warnings []string
	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}},
		func(format string, args ...any) { warnings = append(warnings, format) },
	)

	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", warnings)
	}
	if got := strings.Join(results, ", "); !strings.Contains(got, "codex: node/npm installed via nvm") || !strings.Contains(got, "codex: installed") {
		t.Fatalf("unexpected install summary %q", got)
	}
}

func TestEnsureConfiguredAgentsInstalledBootstrapsCurlThenNVM(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": errors.New("exit status 1"),
			"npm install -g @openai/codex":     nil,
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("0\n"),
			fakeAgentNPMDetectKey:                           []byte("install:apt-get\n"),
		},
	}
	var warnings []string
	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}},
		func(format string, args ...any) { warnings = append(warnings, format) },
	)

	if len(warnings) != 0 {
		t.Fatalf("expected no warnings, got %#v", warnings)
	}
	found := false
	for _, script := range client.scripts {
		if strings.Contains(script, "install_curl()") && strings.Contains(script, "apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends bash curl ca-certificates") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected curl bootstrap script, got %#v", client.scripts)
	}
	if got := strings.Join(results, ", "); !strings.Contains(got, "codex: node/npm installed via nvm") || !strings.Contains(got, "codex: installed") {
		t.Fatalf("unexpected install summary %q", got)
	}
}

func TestEnsureConfiguredAgentsInstalledSkipsBootstrapWhenNodeAndNpmAlreadyPresent(t *testing.T) {
	client := &fakeAgentExecClient{
		results: map[string]error{
			"command -v codex >/dev/null 2>&1": errors.New("exit status 1"),
			"command -v npm >/dev/null 2>&1":   nil,
			"npm install -g @openai/codex":     nil,
		},
		outputs: map[string][]byte{
			`node -p 'process.versions.node.split(".")[0]'`: []byte("20\n"),
		},
	}

	results := ensureConfiguredAgentsInstalled(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}},
		func(string, ...any) {},
	)

	if got := strings.Join(results, ", "); strings.Contains(got, "node/npm installed via") || !strings.Contains(got, "codex: installed") {
		t.Fatalf("unexpected install summary %q", got)
	}
	for _, script := range client.scripts {
		if looksLikeAgentNPMDetectScript(script) {
			t.Fatalf("did not expect npm detect script when node/npm are already present")
		}
	}
}

func TestEnsureConfiguredAgentAuthStagesLocalFile(t *testing.T) {
	localDir := t.TempDir()
	localAuth := filepath.Join(localDir, "auth.json")
	if err := os.WriteFile(localAuth, []byte(`{"token":"abc"}`), 0o600); err != nil {
		t.Fatalf("write local auth: %v", err)
	}
	client := &fakeAgentExecClient{
		results: map[string]error{},
		outputs: map[string][]byte{
			`if [ -L "$HOME"/'.codex/auth.json' ]; then printf symlink; elif [ -e "$HOME"/'.codex/auth.json' ]; then printf file; else printf missing; fi`: []byte("missing"),
		},
	}

	results := ensureConfiguredAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		false,
		[]config.AgentSpec{{Name: "codex", Auth: &config.AgentAuth{LocalPath: localAuth}}},
		func(string, ...any) {},
	)

	if len(results) != 1 || results[0] != "codex: auth staged" {
		t.Fatalf("unexpected results %#v", results)
	}
	if len(client.copyCalls) != 1 || client.copyCalls[0] != localAuth+"->/run/okdev/agents/codex/auth" {
		t.Fatalf("unexpected copy calls %#v", client.copyCalls)
	}
	if len(client.scripts) != 3 {
		t.Fatalf("expected 3 scripts, got %#v", client.scripts)
	}
	if !strings.Contains(client.scripts[1], `mkdir -p '/run/okdev/agents/codex' "$HOME"/'.codex'`) {
		t.Fatalf("unexpected setup script %q", client.scripts[1])
	}
	if !strings.Contains(client.scripts[2], `ln -sfn '/run/okdev/agents/codex/auth' "$HOME"/'.codex/auth.json'`) {
		t.Fatalf("unexpected finalize script %q", client.scripts[2])
	}
}

func TestEnsureConfiguredAgentAuthDoesNotClobberExistingRemoteFile(t *testing.T) {
	localDir := t.TempDir()
	localAuth := filepath.Join(localDir, "auth.json")
	if err := os.WriteFile(localAuth, []byte(`{"token":"abc"}`), 0o600); err != nil {
		t.Fatalf("write local auth: %v", err)
	}
	client := &fakeAgentExecClient{
		results: map[string]error{},
		outputs: map[string][]byte{
			`if [ -L "$HOME"/'.codex/auth.json' ]; then printf symlink; elif [ -e "$HOME"/'.codex/auth.json' ]; then printf file; else printf missing; fi`: []byte("file"),
		},
	}

	results := ensureConfiguredAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		false,
		[]config.AgentSpec{{Name: "codex", Auth: &config.AgentAuth{LocalPath: localAuth}}},
		func(string, ...any) {},
	)

	if len(results) != 1 || results[0] != "codex: remote auth already present" {
		t.Fatalf("unexpected results %#v", results)
	}
	if len(client.copyCalls) != 0 {
		t.Fatalf("expected no copy calls, got %#v", client.copyCalls)
	}
	if len(client.scripts) != 1 {
		t.Fatalf("expected only state check script, got %#v", client.scripts)
	}
}

func TestEnsureConfiguredAgentAuthSkipsShareableSessions(t *testing.T) {
	client := &fakeAgentExecClient{results: map[string]error{}}
	var warnings []string

	results := ensureConfiguredAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		true,
		[]config.AgentSpec{{Name: "codex"}},
		func(format string, args ...any) { warnings = append(warnings, format) },
	)

	if len(results) != 1 || results[0] != "codex: skipped (shareable session)" {
		t.Fatalf("unexpected results %#v", results)
	}
	if len(warnings) != 1 || !strings.Contains(warnings[0], "skipping %s auth staging") {
		t.Fatalf("unexpected warnings %#v", warnings)
	}
	if len(client.scripts) != 0 || len(client.copyCalls) != 0 {
		t.Fatalf("expected no remote operations, got scripts=%#v copyCalls=%#v", client.scripts, client.copyCalls)
	}
}

func TestEnsureConfiguredAgentAuthWarnsForEnvOnlySource(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "secret")
	client := &fakeAgentExecClient{results: map[string]error{}}

	results := ensureConfiguredAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		false,
		[]config.AgentSpec{{Name: "claude-code", Auth: &config.AgentAuth{Env: "ANTHROPIC_API_KEY"}}},
		func(string, ...any) {},
	)

	if len(results) != 1 || results[0] != "claude-code: env configured (manual login still required)" {
		t.Fatalf("unexpected results %#v", results)
	}
	if len(client.scripts) != 0 || len(client.copyCalls) != 0 {
		t.Fatalf("expected no remote operations, got scripts=%#v copyCalls=%#v", client.scripts, client.copyCalls)
	}
}

func TestCleanupConfiguredAgentAuth(t *testing.T) {
	client := &fakeAgentExecClient{results: map[string]error{}}

	results := cleanupConfiguredAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		[]config.AgentSpec{{Name: "codex"}},
		func(string, ...any) {},
	)

	if len(results) != 1 || results[0] != "codex: auth cleaned" {
		t.Fatalf("unexpected results %#v", results)
	}
	if len(client.scripts) != 1 {
		t.Fatalf("expected one cleanup script, got %#v", client.scripts)
	}
	if !strings.Contains(client.scripts[0], `if [ -L "$HOME"/'.codex/auth.json' ]; then rm -f "$HOME"/'.codex/auth.json'; fi && rm -rf '/run/okdev/agents/codex'`) {
		t.Fatalf("unexpected cleanup script %q", client.scripts[0])
	}
}

func TestAgentRemotePathExprEscapesHomeSuffix(t *testing.T) {
	got := agentRemotePathExpr("$HOME/my dir/auth.json")
	want := `"$HOME"/'my dir/auth.json'`
	if got != want {
		t.Fatalf("agentRemotePathExpr() = %q, want %q", got, want)
	}
}

func TestCleanupAgentAuthEscapesHomeSuffix(t *testing.T) {
	client := &fakeAgentExecClient{results: map[string]error{}}

	if err := cleanupAgentAuth(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		agentcatalog.Spec{Name: "custom", RemoteAuthPath: "$HOME/my dir/auth.json"},
	); err != nil {
		t.Fatalf("cleanupAgentAuth: %v", err)
	}
	if len(client.scripts) != 1 {
		t.Fatalf("expected one cleanup script, got %#v", client.scripts)
	}
	if !strings.Contains(client.scripts[0], `if [ -L "$HOME"/'my dir/auth.json' ]; then rm -f "$HOME"/'my dir/auth.json'; fi`) {
		t.Fatalf("unexpected cleanup script %q", client.scripts[0])
	}
}

func TestLinkAgentBinaryQuotesBinaryName(t *testing.T) {
	client := &fakeAgentExecClient{results: map[string]error{}}

	if err := linkAgentBinary(
		context.Background(),
		client,
		"default",
		"pod",
		"dev",
		agentcatalog.Spec{Name: "custom", Binary: "my tool", InstallCommand: "npm install -g custom"},
	); err != nil {
		t.Fatalf("linkAgentBinary: %v", err)
	}
	if len(client.scripts) != 1 {
		t.Fatalf("expected one script, got %#v", client.scripts)
	}
	if !strings.Contains(client.scripts[0], "binary='my tool'") {
		t.Fatalf("unexpected link script %q", client.scripts[0])
	}
}

func TestSummarizeAgentResults(t *testing.T) {
	got := summarizeAgentResults(
		[]string{"claude-code: present", "codex: installed"},
		[]string{"codex: auth staged", "gemini: env configured (manual login still required)"},
	)
	want := "claude-code: present; codex: installed, auth staged; gemini: env configured (manual login still required)"
	if got != want {
		t.Fatalf("summarizeAgentResults() = %q, want %q", got, want)
	}
}
