package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	agentcatalog "github.com/acmore/okdev/internal/agent"
	"github.com/acmore/okdev/internal/config"
	"github.com/acmore/okdev/internal/output"
	syncengine "github.com/acmore/okdev/internal/sync"
	"github.com/spf13/cobra"
)

type agentExecClient interface {
	ExecShInContainer(context.Context, string, string, string, string) ([]byte, error)
	CopyToPodInContainer(context.Context, string, string, string, string, string) error
}

type agentListRow struct {
	Name       string `json:"name"`
	Binary     string `json:"binary"`
	Installed  bool   `json:"installed"`
	AuthEnv    string `json:"authEnv,omitempty"`
	AuthPath   string `json:"authPath,omitempty"`
	AuthStaged bool   `json:"authStaged"`
}

func newAgentCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "agent",
		Short: "Manage coding agents in the session container",
	}
	cmd.AddCommand(newAgentListCmd(opts))
	return cmd
}

func newAgentListCmd(opts *Options) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list [session]",
		Short: "List configured agents and install status",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			applySessionArg(opts, args)
			cc, err := resolveCommandContext(opts, resolveSessionName)
			if err != nil {
				return err
			}
			if err := ensureExistingSessionOwnership(opts, cc.kube, cc.namespace, cc.sessionName, true); err != nil {
				return err
			}
			target, err := resolveTargetRef(cmd.Context(), opts, cc.cfg, cc.namespace, cc.sessionName, cc.kube)
			if err != nil {
				return err
			}
			rows, err := configuredAgentStatusRows(cmd.Context(), cc.kube, cc.namespace, target.PodName, target.Container, cc.cfg.Spec.Agents)
			if err != nil {
				return err
			}
			if opts.Output == "json" {
				return outputJSON(cmd.OutOrStdout(), rows)
			}
			if len(rows) == 0 {
				fmt.Fprintln(cmd.OutOrStdout(), "No agents configured")
				return nil
			}
			table := make([][]string, 0, len(rows))
			for _, row := range rows {
				installed := "no"
				if row.Installed {
					installed = "yes"
				}
				authStaged := "no"
				if row.AuthStaged {
					authStaged = "yes"
				}
				table = append(table, []string{row.Name, row.Binary, installed, authStaged})
			}
			output.PrintTable(cmd.OutOrStdout(), []string{"NAME", "BINARY", "INSTALLED", "AUTH STAGED"}, table)
			return nil
		},
	}
	return cmd
}

func configuredAgentStatusRows(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec) ([]agentListRow, error) {
	rows := make([]agentListRow, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		installed, err := agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil {
			return nil, fmt.Errorf("check %s install status: %w", spec.Name, err)
		}
		row := agentListRow{
			Name:      spec.Name,
			Binary:    spec.Binary,
			Installed: installed,
		}
		if agent.Auth != nil {
			row.AuthEnv = strings.TrimSpace(agent.Auth.Env)
			row.AuthPath = strings.TrimSpace(agent.Auth.LocalPath)
		}
		if spec.RemoteAuthPath != "" {
			staged, err := agentAuthStaged(ctx, client, namespace, pod, container, spec.RemoteAuthPath)
			if err != nil {
				return nil, fmt.Errorf("check %s auth status: %w", spec.Name, err)
			}
			row.AuthStaged = staged
		}
		rows = append(rows, row)
	}
	return rows, nil
}

func ensureConfiguredAgentsInstalled(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		installed, err := agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil {
			warnf("failed to check %s install status: %v", spec.Name, err)
			results = append(results, spec.Name+": check failed")
			continue
		}
		if installed {
			results = append(results, spec.Name+": present")
			continue
		}
		npmInstalled, err := agentBinaryInstalled(ctx, client, namespace, pod, container, "npm")
		if err != nil {
			warnf("failed to check npm before installing %s: %v", spec.Name, err)
			results = append(results, spec.Name+": npm check failed")
			continue
		}
		if !npmInstalled {
			npmResult, npmErr := ensureAgentNPMInstalled(ctx, client, namespace, pod, container)
			if npmErr != nil {
				warnf("skipping %s install: %v", spec.Name, npmErr)
				results = append(results, spec.Name+": skipped (npm missing)")
				continue
			}
			if strings.TrimSpace(npmResult) != "" {
				results = append(results, spec.Name+": "+npmResult)
			}
		}
		if _, err := client.ExecShInContainer(ctx, namespace, pod, container, spec.InstallCommand); err != nil {
			warnf("failed to install %s: %v", spec.Name, err)
			results = append(results, spec.Name+": install failed")
			continue
		}
		installed, err = agentBinaryInstalled(ctx, client, namespace, pod, container, spec.Binary)
		if err != nil || !installed {
			if err != nil {
				warnf("failed to verify %s after install: %v", spec.Name, err)
			} else {
				warnf("%s install completed but binary %q is still unavailable", spec.Name, spec.Binary)
			}
			results = append(results, spec.Name+": verification failed")
			continue
		}
		results = append(results, spec.Name+": installed")
	}
	return results
}

const agentNPMDetectScript = `set -eu
if command -v npm >/dev/null 2>&1; then
  echo present:none
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo no-root:none
  exit 0
fi
installer="none"
if command -v apk >/dev/null 2>&1; then
  installer="apk"
elif command -v apt-get >/dev/null 2>&1; then
  installer="apt-get"
elif command -v apt >/dev/null 2>&1; then
  installer="apt"
elif command -v dnf >/dev/null 2>&1; then
  installer="dnf"
elif command -v microdnf >/dev/null 2>&1; then
  installer="microdnf"
elif command -v yum >/dev/null 2>&1; then
  installer="yum"
fi
if [ "$installer" != "none" ]; then
  echo install:${installer}
else
  echo unavailable:none
fi
`

const agentNPMInstallScript = `set -eu
installer="${OKDEV_NPM_INSTALLER:-none}"
if command -v npm >/dev/null 2>&1; then
  echo "__OKDEV_NPM_STATUS__=installed:${installer}"
  exit 0
fi
if [ "$(id -u)" != "0" ]; then
  echo "__OKDEV_NPM_STATUS__=no-root:none"
  exit 0
fi
if [ "$installer" = "apk" ]; then
  apk add --no-cache nodejs npm >/dev/null 2>&1 || true
elif [ "$installer" = "apt-get" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt-get -o DPkg::Lock::Timeout=10 update >/dev/null 2>&1 && apt-get -o DPkg::Lock::Timeout=10 install -y --no-install-recommends nodejs npm >/dev/null 2>&1 || true
elif [ "$installer" = "apt" ]; then
  export DEBIAN_FRONTEND=noninteractive
  apt update >/dev/null 2>&1 && apt install -y --no-install-recommends nodejs npm >/dev/null 2>&1 || true
elif [ "$installer" = "dnf" ]; then
  dnf install -y nodejs npm >/dev/null 2>&1 || true
elif [ "$installer" = "microdnf" ]; then
  microdnf install -y nodejs npm >/dev/null 2>&1 || true
elif [ "$installer" = "yum" ]; then
  yum install -y nodejs npm >/dev/null 2>&1 || true
fi
if command -v npm >/dev/null 2>&1; then
  echo "__OKDEV_NPM_STATUS__=installed:${installer}"
  exit 0
fi
echo "__OKDEV_NPM_STATUS__=unavailable:${installer}"
`

func ensureAgentNPMInstalled(ctx context.Context, client agentExecClient, namespace, pod, container string) (string, error) {
	status, installer, err := detectAgentNPM(ctx, client, namespace, pod, container)
	if err != nil {
		return "", err
	}
	switch status {
	case "present":
		return "", nil
	case "no-root":
		return "", fmt.Errorf("npm is unavailable and the dev container is not running as root")
	case "unavailable":
		return "", fmt.Errorf("npm is unavailable and no supported package manager was found")
	case "install":
	default:
		return "", fmt.Errorf("unexpected npm prepare result %q", status)
	}
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, "export OKDEV_NPM_INSTALLER="+shellQuote(installer)+"; "+agentNPMInstallScript)
	if err != nil {
		return "", err
	}
	status, installer = parseAgentNPMStatus(string(out))
	switch status {
	case "installed":
		if installer != "" && installer != "none" {
			return "npm installed via " + installer, nil
		}
		return "npm installed", nil
	case "no-root":
		return "", fmt.Errorf("npm is unavailable and the dev container is not running as root")
	case "unavailable":
		if installer != "" && installer != "none" {
			return "", fmt.Errorf("npm is unavailable after best-effort install via %s", installer)
		}
		return "", fmt.Errorf("npm is unavailable after best-effort install")
	default:
		return "", fmt.Errorf("unexpected npm install result %q", strings.TrimSpace(string(out)))
	}
}

func detectAgentNPM(ctx context.Context, client agentExecClient, namespace, pod, container string) (status, installer string, err error) {
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, agentNPMDetectScript)
	if err != nil {
		return "", "", err
	}
	status, installer, _ = strings.Cut(strings.TrimSpace(string(out)), ":")
	return strings.TrimSpace(status), strings.TrimSpace(installer), nil
}

func parseAgentNPMStatus(raw string) (status, installer string) {
	for _, line := range strings.Split(raw, "\n") {
		text := strings.TrimSpace(line)
		if !strings.HasPrefix(text, "__OKDEV_NPM_STATUS__=") {
			continue
		}
		payload := strings.TrimPrefix(text, "__OKDEV_NPM_STATUS__=")
		status, installer, _ = strings.Cut(payload, ":")
		return strings.TrimSpace(status), strings.TrimSpace(installer)
	}
	return "", ""
}

func ensureConfiguredAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, shareable bool, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok {
			continue
		}
		if shareable {
			warnf("skipping %s auth staging: shareable sessions are not a strong trust boundary", spec.Name)
			results = append(results, spec.Name+": skipped (shareable session)")
			continue
		}
		result, err := ensureAgentAuthStaged(ctx, client, namespace, pod, container, agent, spec)
		if err != nil {
			warnf("failed to stage %s auth: %v", spec.Name, err)
			results = append(results, spec.Name+": staging failed")
			continue
		}
		results = append(results, spec.Name+": "+result)
	}
	return results
}

func agentBinaryInstalled(ctx context.Context, client agentExecClient, namespace, pod, container, binary string) (bool, error) {
	if strings.TrimSpace(binary) == "" {
		return false, nil
	}
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, "command -v "+binary+" >/dev/null 2>&1")
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "exit code 127") || strings.Contains(msg, "exit status 127") || strings.Contains(msg, "exit code 1") || strings.Contains(msg, "exit status 1") {
		return false, nil
	}
	return false, err
}

func ensureAgentAuthStaged(ctx context.Context, client agentExecClient, namespace, pod, container string, agent config.AgentSpec, spec agentcatalog.Spec) (string, error) {
	if agent.Auth == nil {
		return "no auth configured", nil
	}
	localPath, found, err := resolveAgentLocalAuth(agent.Auth.LocalPath)
	if err != nil {
		return "", err
	}
	if !found {
		if env := strings.TrimSpace(agent.Auth.Env); env != "" && strings.TrimSpace(os.Getenv(env)) != "" {
			return "env configured (manual login still required)", nil
		}
		return "no local auth found", nil
	}
	stageDir := agentRemoteStageDir(spec.Name)
	stagePath := path.Join(stageDir, "auth")
	remotePath := strings.TrimSpace(spec.RemoteAuthPath)
	if remotePath == "" {
		return "", fmt.Errorf("remote auth path is not defined")
	}
	state, err := agentRemoteAuthState(ctx, client, namespace, pod, container, remotePath)
	if err != nil {
		return "", fmt.Errorf("inspect remote auth path: %w", err)
	}
	if state == "file" {
		return "remote auth already present", nil
	}
	setupScript := fmt.Sprintf(
		"mkdir -p %s %s && install -m 600 /dev/null %s",
		syncengine.ShellEscape(stageDir),
		agentRemotePathExpr(path.Dir(remotePath)),
		syncengine.ShellEscape(stagePath),
	)
	if _, err := client.ExecShInContainer(ctx, namespace, pod, container, setupScript); err != nil {
		return "", fmt.Errorf("prepare remote auth paths: %w", err)
	}
	if err := client.CopyToPodInContainer(ctx, namespace, localPath, pod, container, stagePath); err != nil {
		return "", fmt.Errorf("copy auth file: %w", err)
	}
	finalizeScript := fmt.Sprintf(
		"chmod 600 %s && ln -sfn %s %s",
		syncengine.ShellEscape(stagePath),
		syncengine.ShellEscape(stagePath),
		agentRemotePathExpr(remotePath),
	)
	if _, err := client.ExecShInContainer(ctx, namespace, pod, container, finalizeScript); err != nil {
		return "", fmt.Errorf("link remote auth path: %w", err)
	}
	return "auth staged", nil
}

func cleanupConfiguredAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, agents []config.AgentSpec, warnf func(string, ...any)) []string {
	results := make([]string, 0, len(agents))
	for _, agent := range agents {
		spec, ok := agentcatalog.Lookup(agent.Name)
		if !ok || strings.TrimSpace(spec.RemoteAuthPath) == "" {
			continue
		}
		if err := cleanupAgentAuth(ctx, client, namespace, pod, container, spec); err != nil {
			warnf("failed to clean %s auth: %v", spec.Name, err)
			results = append(results, spec.Name+": cleanup failed")
			continue
		}
		results = append(results, spec.Name+": auth cleaned")
	}
	return results
}

func resolveAgentLocalAuth(localPath string) (string, bool, error) {
	if strings.TrimSpace(localPath) == "" {
		return "", false, nil
	}
	resolved, err := config.ResolveLocalAgentPath(localPath)
	if err != nil {
		return "", false, err
	}
	info, err := os.Stat(resolved)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	if info.IsDir() {
		return "", false, fmt.Errorf("local auth path %q is a directory", resolved)
	}
	return resolved, true, nil
}

func agentAuthStaged(ctx context.Context, client agentExecClient, namespace, pod, container, remotePath string) (bool, error) {
	if strings.TrimSpace(remotePath) == "" {
		return false, nil
	}
	script := "test -f " + agentRemotePathExpr(remotePath)
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	if err == nil {
		return true, nil
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "exit code 1") || strings.Contains(msg, "exit status 1") {
		return false, nil
	}
	return false, err
}

func agentRemoteStageDir(agentName string) string {
	return "/run/okdev/agents/" + agentName
}

func agentRemoteAuthState(ctx context.Context, client agentExecClient, namespace, pod, container, remotePath string) (string, error) {
	if strings.TrimSpace(remotePath) == "" {
		return "missing", nil
	}
	script := fmt.Sprintf("if [ -L %s ]; then printf symlink; elif [ -e %s ]; then printf file; else printf missing; fi", agentRemotePathExpr(remotePath), agentRemotePathExpr(remotePath))
	out, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func cleanupAgentAuth(ctx context.Context, client agentExecClient, namespace, pod, container string, spec agentcatalog.Spec) error {
	stageDir := agentRemoteStageDir(spec.Name)
	remotePath := strings.TrimSpace(spec.RemoteAuthPath)
	if remotePath == "" {
		return nil
	}
	script := fmt.Sprintf(
		"if [ -L %s ]; then rm -f %s; fi && rm -rf %s",
		agentRemotePathExpr(remotePath),
		agentRemotePathExpr(remotePath),
		syncengine.ShellEscape(stageDir),
	)
	_, err := client.ExecShInContainer(ctx, namespace, pod, container, script)
	return err
}

func agentRemotePathExpr(remotePath string) string {
	trimmed := strings.TrimSpace(remotePath)
	switch {
	case trimmed == "$HOME":
		return "\"$HOME\""
	case strings.HasPrefix(trimmed, "$HOME/"):
		return "\"$HOME\"/" + shellEscapeBare(strings.TrimPrefix(trimmed, "$HOME/"))
	default:
		return syncengine.ShellEscape(trimmed)
	}
}

func shellEscapeBare(v string) string {
	escaped := syncengine.ShellEscape(v)
	if len(escaped) >= 2 && escaped[0] == '\'' && escaped[len(escaped)-1] == '\'' {
		return escaped[1 : len(escaped)-1]
	}
	return escaped
}
